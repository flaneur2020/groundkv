package dst

import (
	"errors"
	"io"
	"math/rand"
	"os"
	"sync"

	"github.com/flaneur2020/groundkv/engine"
)

const simSectorSize = 64

var (
	ErrInjectedRead  = errors.New("dst: injected read I/O error")
	ErrInjectedWrite = errors.New("dst: injected write I/O error")
	ErrInjectedSync  = errors.New("dst: injected sync I/O error")
)

// FaultConfig controls deterministic I/O fault injection probabilities on SimDisk.
type FaultConfig struct {
	FailReadProb  float64
	FailWriteProb float64
	FailSyncProb  float64
}

type sectorWrite struct {
	off  int64
	data []byte
}

// PowerLossStyle selects how SimDisk mutates unsynced pending sectors on power loss.
type PowerLossStyle int

const (
	PowerLossDropAll PowerLossStyle = iota
	PowerLossRandomSubset
	PowerLossReorderedMeta
	PowerLossTornMeta
)

// PowerLossReport summarizes what happened to pending sectors during a crash.
type PowerLossReport struct {
	Style        PowerLossStyle
	SectorsTotal int
	SectorsKept  int
	SectorsTorn  int
	SectorsDrop  int
	MetaTouched  bool
	MetaTorn     bool
	DataDropped  bool
}

// SimDisk is a deterministic, sector-level fault-injecting engine.Storage.
// Every WriteAt is split into 64-byte sectors so simulated power losses can
// independently drop, reorder, or tear individual sectors across data pages,
// multi-page freelist chains, and dual meta pages.
type SimDisk struct {
	mu          sync.RWMutex
	rng         *rand.Rand
	faults      FaultConfig
	faultsMuted bool

	working []byte
	durable []byte
	pending []sectorWrite
	closed  bool

	ReadErrs  int
	WriteErrs int
	SyncErrs  int
}

var _ engine.Storage = (*SimDisk)(nil)

// NewSimDisk constructs a fresh empty SimDisk driven by rng.
func NewSimDisk(rng *rand.Rand, faults FaultConfig) *SimDisk {
	return &SimDisk{
		rng:    rng,
		faults: faults,
	}
}

// SetFaultsMuted temporarily suppresses probabilistic I/O errors during
// out-of-band oracle/invariant verification reads.
func (d *SimDisk) SetFaultsMuted(muted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.faultsMuted = muted
}

func (d *SimDisk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	if !d.faultsMuted && d.faults.FailReadProb > 0 && d.rng.Float64() < d.faults.FailReadProb {
		d.ReadErrs++
		return 0, ErrInjectedRead
	}
	if off < 0 {
		return 0, errors.New("dst: negative read offset")
	}
	if off >= int64(len(d.working)) {
		return 0, io.EOF
	}
	n := copy(p, d.working[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (d *SimDisk) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, errors.New("dst: negative write offset")
	}
	if !d.faultsMuted && d.faults.FailWriteProb > 0 && d.rng.Float64() < d.faults.FailWriteProb {
		d.WriteErrs++
		// Deterministically simulate either zero bytes written or a torn partial
		// write into the OS buffer on I/O error (never touching already-durable data).
		if len(p) > 32 && d.rng.Intn(2) == 0 {
			half := len(p) / 2
			torn := make([]byte, half)
			copy(torn, p[:half])
			torn[len(torn)-1] ^= 0xFF
			applySlice(&d.working, off, torn)
			d.appendSectorsLocked(off, torn)
		}
		return 0, ErrInjectedWrite
	}

	applySlice(&d.working, off, p)
	d.appendSectorsLocked(off, p)
	return len(p), nil
}

func (d *SimDisk) appendSectorsLocked(off int64, p []byte) {
	for pos := 0; pos < len(p); pos += simSectorSize {
		end := pos + simSectorSize
		if end > len(p) {
			end = len(p)
		}
		cp := make([]byte, end-pos)
		copy(cp, p[pos:end])
		d.pending = append(d.pending, sectorWrite{
			off:  off + int64(pos),
			data: cp,
		})
	}
}

func (d *SimDisk) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return os.ErrClosed
	}
	if !d.faultsMuted && d.faults.FailSyncProb > 0 && d.rng.Float64() < d.faults.FailSyncProb {
		d.SyncErrs++
		return ErrInjectedSync
	}
	d.durable = make([]byte, len(d.working))
	copy(d.durable, d.working)
	d.pending = nil
	return nil
}

func (d *SimDisk) Size() (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	return int64(len(d.working)), nil
}

func (d *SimDisk) Truncate(size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return os.ErrClosed
	}
	if size < 0 {
		return errors.New("dst: negative truncate size")
	}
	if int(size) < len(d.working) {
		d.working = d.working[:size]
	} else if int(size) > len(d.working) {
		grown := make([]byte, size)
		copy(grown, d.working)
		d.working = grown
	}
	return nil
}

func (d *SimDisk) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// CloneDurable returns a clean read-only SimDisk (faults muted) initialized
// solely from the durable synced image without modifying d.
func (d *SimDisk) CloneDurable() *SimDisk {
	d.mu.RLock()
	defer d.mu.RUnlock()
	img := make([]byte, len(d.durable))
	copy(img, d.durable)
	return &SimDisk{
		rng:         d.rng,
		faultsMuted: true,
		working:     img,
		durable:     append([]byte(nil), img...),
	}
}

// CloneDurableWithBitRot returns a cloned durable SimDisk with a single byte
// flipped inside each listed page ID to simulate silent storage bit-rot /
// latent sector errors without mutating d.
func (d *SimDisk) CloneDurableWithBitRot(pageSize int, pgids ...engine.PageID) (*SimDisk, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	img := make([]byte, len(d.durable))
	copy(img, d.durable)
	flipped := false
	for _, pgid := range pgids {
		// Flip byte at offset 16 (inside page header txid, covered by CRC32).
		off := int(pgid)*pageSize + 16
		if off >= 0 && off < len(img) {
			img[off] ^= 0x5A
			flipped = true
		}
	}
	return &SimDisk{
		rng:         d.rng,
		faultsMuted: true,
		working:     img,
		durable:     append([]byte(nil), img...),
	}, flipped
}

// PowerLoss simulates a sudden power failure on d under the specified style,
// closing d and returning a fresh rebooted SimDisk reflecting the surviving
// on-disk sectors.
func (d *SimDisk) PowerLoss(style PowerLossStyle, pageSize int) (*SimDisk, PowerLossReport) {
	d.mu.Lock()
	defer d.mu.Unlock()

	img := make([]byte, len(d.durable))
	copy(img, d.durable)

	metaLimit := int64(2 * pageSize)
	rep := PowerLossReport{
		Style:        style,
		SectorsTotal: len(d.pending),
	}

	switch style {
	case PowerLossDropAll:
		rep.SectorsDrop = len(d.pending)
		for _, sw := range d.pending {
			if sw.off >= metaLimit {
				rep.DataDropped = true
			}
		}

	case PowerLossReorderedMeta:
		for _, sw := range d.pending {
			if sw.off < metaLimit {
				applySlice(&img, sw.off, sw.data)
				rep.SectorsKept++
				rep.MetaTouched = true
			} else {
				rep.SectorsDrop++
				rep.DataDropped = true
			}
		}

	case PowerLossTornMeta:
		for _, sw := range d.pending {
			if sw.off < metaLimit {
				torn := make([]byte, len(sw.data))
				half := len(sw.data) / 2
				copy(torn[:half], sw.data[:half])
				if len(torn) > 0 {
					torn[len(torn)-1] ^= 0xFF
				}
				applySlice(&img, sw.off, torn)
				rep.SectorsTorn++
				rep.MetaTouched = true
				rep.MetaTorn = true
			} else {
				rep.SectorsDrop++
				rep.DataDropped = true
			}
		}

	case PowerLossRandomSubset:
		// Independent per-sector fate: 40% drop, 40% persist intact, 20% torn!
		for _, sw := range d.pending {
			isMeta := sw.off < metaLimit
			roll := d.rng.Intn(10)
			switch {
			case roll < 4:
				rep.SectorsDrop++
				if !isMeta {
					rep.DataDropped = true
				}
			case roll < 8:
				applySlice(&img, sw.off, sw.data)
				rep.SectorsKept++
				if isMeta {
					rep.MetaTouched = true
				}
			default:
				torn := make([]byte, len(sw.data))
				half := len(sw.data) / 2
				copy(torn[:half], sw.data[:half])
				if len(torn) > 0 {
					torn[len(torn)-1] ^= 0xAA
				}
				applySlice(&img, sw.off, torn)
				rep.SectorsTorn++
				if isMeta {
					rep.MetaTouched = true
					rep.MetaTorn = true
				}
			}
		}
	}

	d.closed = true
	next := &SimDisk{
		rng:       d.rng,
		faults:    d.faults,
		working:   img,
		durable:   append([]byte(nil), img...),
		ReadErrs:  d.ReadErrs,
		WriteErrs: d.WriteErrs,
		SyncErrs:  d.SyncErrs,
	}
	return next, rep
}

func applySlice(dst *[]byte, off int64, data []byte) {
	end := int(off) + len(data)
	if end > len(*dst) {
		grown := make([]byte, end)
		copy(grown, *dst)
		*dst = grown
	}
	copy((*dst)[off:], data)
}
