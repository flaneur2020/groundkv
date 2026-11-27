package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/flaneur2020/groundkv/engine/core"
)

// FuzzDecodePages hammers the low-level page header, meta, leaf, branch, and
// freelist binary decoders with arbitrary mutated byte slices to cover every
// defensive truncation, flag, count, and checksum guard without panicking.
func FuzzDecodePages(f *testing.F) {
	const ps = 256
	c := GoCore()

	// Valid meta page (even slot 0 with even TxID 2) + parity-mismatch seed (slot 0 with odd TxID 1)
	f.Add(encodeMetaPage(c, ps, 0, Meta{
		Magic: core.MetaMagic, Version: core.MetaVersion, PageSize: ps,
		Root: 2, HighWater: 4, TxID: 2, Freelist: 3,
	}), uint64(0))
	f.Add(encodeMetaPage(c, ps, 0, Meta{
		Magic: core.MetaMagic, Version: core.MetaVersion, PageSize: ps,
		Root: 2, HighWater: 4, TxID: 1, Freelist: 3,
	}), uint64(0))

	// Valid leaf page
	if leaf, err := encodeLeafPage(c, ps, 2, 1, []LeafEntry{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
	}); err == nil {
		f.Add(leaf, uint64(2))
	}

	// Valid branch page
	if br, err := encodeBranchPage(c, ps, 3, 1, []BranchEntry{
		{Key: []byte("a"), Child: 2},
		{Key: []byte("m"), Child: 4},
	}); err == nil {
		f.Add(br, uint64(3))
	}

	// Valid freelist page
	if fl, err := encodeFreelistPage(c, ps, 4, 2, 5, []PageID{2, 3}); err == nil {
		f.Add(fl, uint64(4))
	}

	// Edge cases: empty, short, zeroed
	f.Add([]byte{}, uint64(0))
	f.Add(make([]byte, 12), uint64(0))
	f.Add(make([]byte, ps), uint64(0))

	f.Fuzz(func(t *testing.T, raw []byte, pgid uint64) {
		id := PageID(pgid)
		_, _ = decodeMetaPage(c, raw, id)

		h, err := verifyPageHeader(c, raw, id)
		if err != nil {
			// Also test decoders with synthetic headers even when CRC fails,
			// exercising malformed count/length guards directly.
			if len(raw) >= core.PageHeaderSize {
				synth := PageHeader{
					PGID:  id,
					Flags: binary.LittleEndian.Uint16(raw[8:10]),
					Count: binary.LittleEndian.Uint16(raw[10:12]),
				}
				_, _ = c.DecodeLeafPage(raw, synth)
				_, _ = c.DecodeBranchPage(raw, synth)
				_, _, _ = c.DecodeFreelistPage(raw, synth)
			}
			return
		}

		switch h.Flags {
		case PageFlagMeta:
			_, _ = decodeMetaPage(c, raw, id)
		case PageFlagLeaf:
			_, _ = c.DecodeLeafPage(raw, h)
		case PageFlagBranch:
			_, _ = c.DecodeBranchPage(raw, h)
		case PageFlagFreelist:
			_, _, _ = c.DecodeFreelistPage(raw, h)
		}
	})
}

// FuzzOpenCorruptStorage feeds mutated full database images into OpenStorage,
// Validate, Scan, InspectMeta, and ReachablePages to ensure corrupt disks
// (cycles, bad pointers, torn metas, unsorted leaves, truncated pages) are
// always rejected cleanly with errors and never cause panics.
func FuzzOpenCorruptStorage(f *testing.F) {
	c := GoCore()

	// Seed 1: fresh empty DB
	stEmpty := NewMemStorage()
	if db, err := OpenStorage(stEmpty, &Options{PageSize: 256}); err == nil {
		f.Add(stEmpty.CloneDurable().working)
		_ = db.Close()
	}

	// Seed 2: 3-level branch-split DB with multi-page freelist chain
	stRich := NewMemStorage()
	opts := &Options{PageSize: 256, MaxLeafKeys: 2, MaxBranchKeys: 2, MaxFreelistIDs: 3}
	if db, err := OpenStorage(stRich, opts); err == nil {
		for i := 0; i < 8; i++ {
			_ = db.Put([]byte(fmt.Sprintf("k%d", i%5)), []byte(fmt.Sprintf("val-%d", i)))
		}
		f.Add(stRich.CloneDurable().working)
		// Seed 3: torn meta 0 fallback image
		stTorn := stRich.CloneDurable()
		stTorn.CorruptPage(0, 256)
		f.Add(stTorn.working)
		// Seed 4: truncated mid-page image
		img := stRich.CloneDurable().working
		if len(img) > 300 {
			f.Add(img[:300])
		}
		_ = db.Close()
	}

	// Seed 5: hand-crafted cyclic branch page (pgid 2 -> child 2) with valid CRC32!
	stCycle := NewMemStorage()
	if db, err := OpenStorage(stCycle, &Options{PageSize: 256}); err == nil {
		_ = db.Close()
		img := stCycle.CloneDurable().working
		brBuf, _ := encodeBranchPage(c, 256, 2, 1, []BranchEntry{{Key: []byte("a"), Child: 2}})
		metaBuf := encodeMetaPage(c, 256, 1, Meta{
			Magic: core.MetaMagic, Version: core.MetaVersion, PageSize: 256,
			Root: 2, HighWater: 3, TxID: 1,
		})
		applyWrite(&img, 256, metaBuf)
		applyWrite(&img, 512, brBuf)
		f.Add(img)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 64*1024 {
			return
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		st := &MemStorage{
			working: cp,
			durable: bytes.Clone(cp),
		}
		db, err := OpenStorage(st, &Options{PageSize: 256})
		if err != nil {
			return
		}
		defer db.Close()

		// Exercise validation, scan, meta inspection, and reachable-page walk.
		_ = db.Validate()
		_ = db.View(func(tx *Tx) error {
			return tx.Scan(nil, nil, func(k, v []byte) bool { return true })
		})
		if s0, s1, err := db.InspectMeta(); err == nil {
			if s0.Valid {
				_, _ = db.ReachablePages(s0.Meta.Root, s0.Meta.Freelist)
			}
			if s1.Valid {
				_, _ = db.ReachablePages(s1.Meta.Root, s1.Meta.Freelist)
			}
		}
	})
}
