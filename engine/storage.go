package engine

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Storage abstracts the underlying page-addressable file store so GroundKV can
// run on real OS files in production and on deterministic, crash-injectable
// storage in unit tests and FizzBee MBT harnesses.
type Storage interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Size() (int64, error)
	Truncate(size int64) error
	Close() error
}

// OSFileStorage backs Storage with a real *os.File.
type OSFileStorage struct {
	f *os.File
}

// OpenOSFile opens or creates a database file at path and acquires an exclusive
// non-blocking advisory lock (flock) to prevent multi-process corruption.
// When creating a new file, it also fsyncs the parent directory so the directory
// entry itself survives power loss on POSIX filesystems.
func OpenOSFile(path string) (*OSFileStorage, error) {
	_, statErr := os.Stat(path)
	isNew := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if isNew {
		if err := syncParentDir(path); err != nil {
			_ = unlockFile(f)
			_ = f.Close()
			return nil, err
		}
	}
	return &OSFileStorage{f: f}, nil
}

func syncParentDir(path string) error {
	dirPath := filepath.Dir(path)
	dir, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (s *OSFileStorage) ReadAt(p []byte, off int64) (int, error)  { return s.f.ReadAt(p, off) }
func (s *OSFileStorage) WriteAt(p []byte, off int64) (int, error) { return s.f.WriteAt(p, off) }
func (s *OSFileStorage) Sync() error                              { return s.f.Sync() }
func (s *OSFileStorage) Truncate(size int64) error                { return s.f.Truncate(size) }

func (s *OSFileStorage) Close() error {
	_ = unlockFile(s.f)
	return s.f.Close()
}

func (s *OSFileStorage) Size() (int64, error) {
	fi, err := s.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// CrashMode selects how unsynced pending writes behave on simulated power loss.
type CrashMode int

const (
	// CrashDropUnsynced drops all unsynced writes (clean rollback to last Sync).
	CrashDropUnsynced CrashMode = iota

	// CrashReorderedMetaOnly simulates disk write reordering: unsynced writes to
	// meta pages (Page 0 and Page 1) are persisted to durable storage while all
	// unsynced B+Tree data page writes are dropped. If the engine omitted the
	// barrier Sync() between data pages and the meta page, this produces a
	// corrupt/dangling root on recovery!
	CrashReorderedMetaOnly

	// CrashTornMetaWrite simulates a torn/partial sector write to any unsynced
	// meta page write (persisting half the page + corrupted bytes into durable
	// storage so CRC32 fails on reopen) while dropping other unsynced writes.
	CrashTornMetaWrite
)

type pendingWrite struct {
	off  int64
	data []byte
}

// MemStorage is an in-memory Storage implementation that tracks every unsynced
// WriteAt separately from synced (durable) bytes. It supports deterministic
// crash modes (drop unsynced, reordered meta flush, torn meta write) for
// durability-grade unit tests and FizzBee MBT.
type MemStorage struct {
	mu        sync.RWMutex
	working   []byte
	durable   []byte
	pending   []pendingWrite
	syncCount int
	closed    bool
	failSync  bool
	failWrite bool
}

// NewMemStorage creates a fresh empty MemStorage.
func NewMemStorage() *MemStorage {
	return &MemStorage{}
}

func (m *MemStorage) ReadAt(p []byte, off int64) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, errors.New("groundkv: negative read offset")
	}
	if off >= int64(len(m.working)) {
		return 0, io.EOF
	}
	n := copy(p, m.working[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *MemStorage) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, errors.New("groundkv: negative write offset")
	}
	if m.failWrite {
		return 0, errors.New("groundkv: injected write failure")
	}
	applyWrite(&m.working, off, p)
	cp := make([]byte, len(p))
	copy(cp, p)
	m.pending = append(m.pending, pendingWrite{off: off, data: cp})
	return len(p), nil
}

func (m *MemStorage) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return os.ErrClosed
	}
	if m.failSync {
		return errors.New("groundkv: injected sync failure")
	}
	m.durable = make([]byte, len(m.working))
	copy(m.durable, m.working)
	m.pending = nil
	m.syncCount++
	return nil
}

func (m *MemStorage) Size() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return 0, os.ErrClosed
	}
	return int64(len(m.working)), nil
}

func (m *MemStorage) Truncate(size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return os.ErrClosed
	}
	if size < 0 {
		return errors.New("groundkv: negative truncate size")
	}
	if int(size) < len(m.working) {
		m.working = m.working[:size]
	} else if int(size) > len(m.working) {
		grown := make([]byte, size)
		copy(grown, m.working)
		m.working = grown
	}
	return nil
}

func (m *MemStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// CloneDurable returns a read-only snapshot MemStorage initialized solely from
// the currently synced durable bytes without closing or modifying m.
func (m *MemStorage) CloneDurable() *MemStorage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	img := make([]byte, len(m.durable))
	copy(img, m.durable)
	return &MemStorage{
		working: img,
		durable: append([]byte(nil), img...),
	}
}

// SimulateCrash simulates power loss dropping all unsynced writes and returns a
// fresh open MemStorage initialized from durable bytes.
func (m *MemStorage) SimulateCrash() *MemStorage {
	return m.SimulateCrashWithMode(CrashDropUnsynced, 0)
}

// SimulateCrashWithMode simulates power loss under the requested CrashMode and
// returns a fresh open MemStorage reflecting the surviving on-disk image.
func (m *MemStorage) SimulateCrashWithMode(mode CrashMode, pageSize int) *MemStorage {
	m.mu.Lock()
	defer m.mu.Unlock()

	img := make([]byte, len(m.durable))
	copy(img, m.durable)

	metaLimit := int64(2 * pageSize)

	switch mode {
	case CrashDropUnsynced:
		// All pending writes vanish.

	case CrashReorderedMetaOnly:
		// Persist only pending writes targeting meta pages (Page 0 / Page 1),
		// dropping all pending B+Tree data page writes.
		for _, pw := range m.pending {
			if pageSize > 0 && pw.off < metaLimit {
				applyWrite(&img, pw.off, pw.data)
			}
		}

	case CrashTornMetaWrite:
		// Simulate a torn sector write to any pending meta page write: write the
		// first half of the page and flip bits in the header/payload so CRC32
		// verification deterministically rejects the torn meta slot.
		for _, pw := range m.pending {
			if pageSize > 0 && pw.off < metaLimit && len(pw.data) >= pageHeaderSize {
				torn := make([]byte, len(pw.data))
				half := len(pw.data) / 2
				copy(torn[:half], pw.data[:half])
				// Flip payload byte to guarantee checksum mismatch even if old/new prefix matched.
				torn[pageHeaderSize] ^= 0xFF
				applyWrite(&img, pw.off, torn)
			}
		}
	}

	m.closed = true
	return &MemStorage{
		working: img,
		durable: append([]byte(nil), img...),
	}
}

// CorruptPage overwrites a single byte at the given page index in both working
// and durable images to test torn-write / meta-page corruption recovery.
func (m *MemStorage) CorruptPage(pgid PageID, pageSize int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	off := int(pgid) * pageSize
	if off+16 < len(m.working) {
		m.working[off+16] ^= 0xFF
	}
	if off+16 < len(m.durable) {
		m.durable[off+16] ^= 0xFF
	}
}

// SetFailSync toggles injected Sync errors for fault-injection tests.
func (m *MemStorage) SetFailSync(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failSync = fail
}

// SetFailWrite toggles injected WriteAt errors for fault-injection tests.
func (m *MemStorage) SetFailWrite(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWrite = fail
}

func applyWrite(dst *[]byte, off int64, data []byte) {
	end := int(off) + len(data)
	if end > len(*dst) {
		grown := make([]byte, end)
		copy(grown, *dst)
		*dst = grown
	}
	copy((*dst)[off:], data)
}
