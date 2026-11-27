package dst

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/flaneur2020/groundkv/engine"
)

func (s *Simulator) probeOptions() *engine.Options {
	cp := s.profile.Options
	cp.Tracer = nil
	return &cp
}

func (s *Simulator) verifyInvariants() error {
	s.disk.SetFaultsMuted(true)
	defer s.disk.SetFaultsMuted(false)

	// 1. Live B+Tree structural & CRC32 validation.
	if err := s.db.Validate(); err != nil {
		return fmt.Errorf("live db.Validate(): %w", err)
	}

	// 2. Live committed map matches exact shadow oracle.
	meta := s.db.Meta()
	if meta.TxID < s.lastSyncedTxID {
		return fmt.Errorf("meta.TxID %d < lastSyncedTxID %d", meta.TxID, s.lastSyncedTxID)
	}
	gotCommitted, err := s.db.ReadTreeAtRoot(meta.Root)
	if err != nil {
		return fmt.Errorf("ReadTreeAtRoot(active): %w", err)
	}
	if !mapsEqual(gotCommitted, s.committed) {
		return fmt.Errorf("committed state mismatch: got %d keys, oracle has %d keys", len(gotCommitted), len(s.committed))
	}

	// 3. Durable disk image MUST always open and validate cleanly (even mid-commit!).
	durableDisk := s.disk.CloneDurable()
	durableDB, err := engine.OpenStorage(durableDisk, s.probeOptions())
	if err != nil {
		return fmt.Errorf("durable image OpenStorage failed: %w", err)
	}
	if err := durableDB.Validate(); err != nil {
		_ = durableDB.Close()
		return fmt.Errorf("durable image Validate failed: %w", err)
	}
	_ = durableDB.Close()

	// 4. Every open MVCC reader sees its exact immutable point-in-time snapshot.
	livePages := make(map[engine.PageID]bool)
	slot0, slot1, err := s.db.InspectMeta()
	if err != nil {
		return fmt.Errorf("InspectMeta: %w", err)
	}
	if !slot0.Valid && !slot1.Valid {
		return errors.New("both meta slots invalid")
	}
	for _, slot := range []engine.MetaSlot{slot0, slot1} {
		if !slot.Valid {
			continue
		}
		pages, err := s.db.ReachablePages(slot.Meta.Root, slot.Meta.Freelist)
		if err != nil {
			return fmt.Errorf("ReachablePages(meta txid=%d): %w", slot.Meta.TxID, err)
		}
		for _, id := range pages {
			livePages[id] = true
		}
	}

	for id, r := range s.readers {
		gotSnap, err := r.tx.All()
		if err != nil {
			return fmt.Errorf("Reader#%d.All() failed: %w", id, err)
		}
		if !mapsEqual(gotSnap, r.expected) {
			return fmt.Errorf("MVCC VIOLATION on Reader#%d (txid=%d): snapshot drifted from oracle", id, r.pinnedTx)
		}
		pages, err := s.db.ReachablePages(r.tx.RootPageID(), 0)
		if err != nil {
			return fmt.Errorf("ReachablePages(Reader#%d): %w", id, err)
		}
		for _, pid := range pages {
			livePages[pid] = true
		}
	}

	// 5. Freelist & MVCC disjointness: NO live page may appear in freelist.free,
	//    and NO pending txid may sit <= ReleaseBound().
	fl := s.db.FreelistState()
	for _, freeID := range fl.FreeIDs {
		if livePages[freeID] {
			return fmt.Errorf("FREELIST USE-AFTER-FREE: live page %d present in freelist.free", freeID)
		}
	}
	bound := s.db.ReleaseBound()
	for _, pt := range fl.PendingTxIDs {
		if pt <= bound {
			return fmt.Errorf("FREELIST WATERMARK VIOLATION: pending txid %d <= releaseBound %d", pt, bound)
		}
	}

	// 6. Periodic non-destructive silent bit-rot (LSE) verification on durable image.
	if s.step > 0 && s.step%30 == 0 {
		if err := s.verifyBitRotDetectionLocked(); err != nil {
			return err
		}
	}

	// 7. Track "sometimes" scenario coverage metrics from real engine state!
	s.observeCoverageLocked(meta, fl)
	return nil
}

// verifyBitRotDetectionLocked clones the durable disk image, flips a bit in a
// live page (both meta slots, active root, freelist chain, or interior leaf),
// and asserts GroundKV always surfaces ErrCorruptedMeta/ErrCorruptedPage cleanly
// rather than silently returning corrupted data or rolling back committed data.
func (s *Simulator) verifyBitRotDetectionLocked() error {
	ps := s.profile.Options.PageSize

	// Case A: Corrupting both meta slots must always fail OpenStorage with ErrCorruptedMeta.
	bothBad, ok := s.disk.CloneDurableWithBitRot(ps, 0, 1)
	if ok {
		if dbBad, err := engine.OpenStorage(bothBad, s.probeOptions()); err == nil {
			_ = dbBad.Close()
			return errors.New("BIT-ROT VIOLATION: OpenStorage succeeded when both meta slots were corrupted")
		} else if !errors.Is(err, engine.ErrCorruptedMeta) {
			return fmt.Errorf("BIT-ROT VIOLATION: expected ErrCorruptedMeta, got %v", err)
		}
		s.cov.Hit(EventBitRotDetected)
	}

	// Case B: Corrupting the durable active root page (when non-empty) must fail loudly
	// on OpenStorage rather than silently reverting to an older meta slot.
	durableProbe := s.disk.CloneDurable()
	probeDB, err := engine.OpenStorage(durableProbe, s.probeOptions())
	if err != nil {
		return fmt.Errorf("bit-rot probe OpenStorage: %w", err)
	}
	dMeta := probeDB.Meta()
	var nonRootPages []engine.PageID
	if dMeta.Root != 0 {
		if pages, pErr := probeDB.ReachablePages(dMeta.Root, 0); pErr == nil {
			for _, pid := range pages {
				if pid != dMeta.Root {
					nonRootPages = append(nonRootPages, pid)
				}
			}
		}
	}
	_ = probeDB.Close()

	if dMeta.Root != 0 {
		rootBad, flipped := s.disk.CloneDurableWithBitRot(ps, dMeta.Root)
		if flipped {
			if dbBad, err := engine.OpenStorage(rootBad, s.probeOptions()); err == nil {
				_ = dbBad.Close()
				return fmt.Errorf("BIT-ROT VIOLATION: OpenStorage succeeded despite corrupt active root page %d", dMeta.Root)
			} else if !errors.Is(err, engine.ErrCorruptedPage) {
				return fmt.Errorf("BIT-ROT VIOLATION: expected ErrCorruptedPage on corrupt root, got %v", err)
			}
			s.cov.Hit(EventBitRotDetected)
		}
	}

	// Case C: Corrupting a non-root reachable page must be caught by Validate().
	if len(nonRootPages) > 0 {
		victim := nonRootPages[s.step%len(nonRootPages)]
		leafBad, flipped := s.disk.CloneDurableWithBitRot(ps, victim)
		if flipped {
			dbBad, err := engine.OpenStorage(leafBad, s.probeOptions())
			if err == nil {
				valErr := dbBad.Validate()
				_ = dbBad.Close()
				if !errors.Is(valErr, engine.ErrCorruptedPage) {
					return fmt.Errorf("BIT-ROT VIOLATION: Validate() did not catch corrupt page %d (err=%v)", victim, valErr)
				}
				s.cov.Hit(EventBitRotDetected)
			}
		}
	}
	return nil
}

func (s *Simulator) observeCoverageLocked(meta engine.Meta, fl engine.FreelistSnapshot) {
	if meta.Root == 0 {
		if s.prevTreeHeight > 0 {
			s.cov.Hit(EventEmptyTree)
		}
		s.prevTreeHeight = 0
	} else {
		height := s.measureTreeHeight(meta.Root)
		if height >= 2 {
			s.cov.Hit(EventLeafSplit)
		}
		if height >= 3 {
			s.cov.Hit(EventBranchSplit)
		}
		if s.prevTreeHeight > height && height > 0 {
			s.cov.Hit(EventRootCollapse)
		}
		s.prevTreeHeight = height
	}

	if meta.Freelist != 0 {
		flPages, err := s.db.ReachablePages(0, meta.Freelist)
		if err == nil && len(flPages) >= 2 {
			s.cov.Hit(EventMultiPageFreelist)
		}
	}
	if fl.ReclaimedHits > 0 {
		s.cov.Hit(EventFreelistReclaim)
	}
	if minR, ok := s.db.MinActiveReaderTxID(); ok && meta.TxID > 1 && minR < meta.TxID-1 && len(fl.PendingTxIDs) > 0 {
		s.cov.Hit(EventReaderBlockedReclaim)
	}
}

func (s *Simulator) countLeafPages(root engine.PageID) int {
	if root == 0 {
		return 0
	}
	pages, err := s.db.ReachablePages(root, 0)
	if err != nil {
		return 0
	}
	leaves := 0
	for _, pid := range pages {
		buf, err := s.db.RawPage(pid)
		if err != nil || len(buf) < 10 {
			continue
		}
		flags := uint16(buf[8]) | uint16(buf[9])<<8
		if flags == engine.PageFlagLeaf {
			leaves++
		}
	}
	return leaves
}

func (s *Simulator) measureTreeHeight(root engine.PageID) int {
	height := 0
	curr := root
	for curr != 0 && height < 16 {
		buf, err := s.db.RawPage(curr)
		if err != nil || len(buf) < 24 {
			break
		}
		flags := uint16(buf[8]) | uint16(buf[9])<<8
		height++
		if flags == engine.PageFlagLeaf {
			break
		}
		if flags != engine.PageFlagBranch || len(buf) < 24+12 {
			break
		}
		// First branch entry: [24:28]=klen, [28:36]=child PageID
		var child uint64
		for i := 0; i < 8; i++ {
			child |= uint64(buf[28+i]) << (8 * i)
		}
		curr = engine.PageID(child)
	}
	return height
}

func (s *Simulator) computeDigest(meta engine.Meta) string {
	h := sha256.New()
	fmt.Fprintf(h, "txid=%d;hw=%d;", meta.TxID, meta.HighWater)
	for _, k := range sortedMapKeys(s.committed) {
		fmt.Fprintf(h, "%x=%s;", k, s.committed[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func expectedRangeScan(m map[string]string, start, end []byte) []string {
	out := make([]string, 0)
	for _, k := range sortedMapKeys(m) {
		kb := []byte(k)
		if len(start) > 0 && bytes.Compare(kb, start) < 0 {
			continue
		}
		if len(end) > 0 && bytes.Compare(kb, end) >= 0 {
			break
		}
		out = append(out, fmt.Sprintf("%x=%s", kb, m[k]))
	}
	return out
}
