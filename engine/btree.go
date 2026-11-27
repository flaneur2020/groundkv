package engine

import (
	"bytes"
	"fmt"
	"sort"
)

// BTreeCore abstracts pure sorted B+Tree node lookup, mutation, and byte-aware
// multi-way splitting.
type BTreeCore interface {
	LeafGet(entries []LeafEntry, key []byte) ([]byte, bool)
	LeafPut(entries []LeafEntry, key, value []byte) []LeafEntry
	LeafDelete(entries []LeafEntry, key []byte) ([]LeafEntry, bool)
	BranchChildIndex(entries []BranchEntry, key []byte) int
	SplitLeafEntries(entries []LeafEntry, pageSize, maxKeys int) [][]LeafEntry
	SplitBranchEntries(entries []BranchEntry, pageSize, maxKeys int) [][]BranchEntry
}

// --- Pure-Go BTreeCore implementation ---

type goBTreeCore struct{}

func (goBTreeCore) LeafGet(entries []LeafEntry, key []byte) ([]byte, bool) {
	idx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) >= 0
	})
	if idx < len(entries) && bytes.Equal(entries[idx].Key, key) {
		return cloneBytes(entries[idx].Value), true
	}
	return nil, false
}

func (goBTreeCore) LeafPut(entries []LeafEntry, key, value []byte) []LeafEntry {
	idx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) >= 0
	})
	if idx < len(entries) && bytes.Equal(entries[idx].Key, key) {
		entries[idx].Value = cloneBytes(value)
		return entries
	}
	entries = append(entries, LeafEntry{})
	copy(entries[idx+1:], entries[idx:])
	entries[idx] = LeafEntry{Key: cloneBytes(key), Value: cloneBytes(value)}
	return entries
}

func (goBTreeCore) LeafDelete(entries []LeafEntry, key []byte) ([]LeafEntry, bool) {
	idx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) >= 0
	})
	if idx >= len(entries) || !bytes.Equal(entries[idx].Key, key) {
		return entries, false
	}
	return append(entries[:idx], entries[idx+1:]...), true
}

func (goBTreeCore) BranchChildIndex(entries []BranchEntry, key []byte) int {
	return branchChildIndex(entries, key)
}

func (goBTreeCore) SplitLeafEntries(entries []LeafEntry, pageSize, maxKeys int) [][]LeafEntry {
	return splitLeafEntries(entries, pageSize, maxKeys)
}

func (goBTreeCore) SplitBranchEntries(entries []BranchEntry, pageSize, maxKeys int) [][]BranchEntry {
	return splitBranchEntries(entries, pageSize, maxKeys)
}

// pageAccess abstracts page reading, COW allocation, and replaced-page freeing for B+Tree operations.
type pageAccess interface {
	core() Core
	pageSize() int
	maxLeafKeys() int
	maxBranchKeys() int
	txid() uint64
	readPage(pgid PageID) ([]byte, PageHeader, error)
	allocPage(encode func(pgid PageID) ([]byte, error)) (PageID, error)
	freePage(pgid PageID)
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

const maxBTreeDepth = 64

func btreeGet(pa pageAccess, root PageID, key []byte) ([]byte, bool, error) {
	if root == 0 {
		return nil, false, nil
	}
	c := pa.core()
	curr := root
	for depth := 0; depth < maxBTreeDepth; depth++ {
		buf, h, err := pa.readPage(curr)
		if err != nil {
			return nil, false, err
		}
		switch h.Flags {
		case PageFlagLeaf:
			entries, err := c.DecodeLeafPage(buf, h)
			if err != nil {
				return nil, false, err
			}
			v, ok := c.LeafGet(entries, key)
			return v, ok, nil

		case PageFlagBranch:
			entries, err := c.DecodeBranchPage(buf, h)
			if err != nil {
				return nil, false, err
			}
			if len(entries) == 0 {
				return nil, false, ErrCorruptedPage
			}
			idx := c.BranchChildIndex(entries, key)
			curr = entries[idx].Child

		default:
			return nil, false, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.Flags, curr)
		}
	}
	return nil, false, fmt.Errorf("%w: max btree depth exceeded during Get", ErrCorruptedPage)
}

// branchChildIndex returns the child index for key using BoltDB's convention:
// the largest index i where entries[i].Key <= key, clamped to 0 if key < entries[0].Key.
func branchChildIndex(entries []BranchEntry, key []byte) int {
	upper := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) > 0
	})
	if upper == 0 {
		return 0
	}
	return upper - 1
}

func btreeScan(pa pageAccess, root PageID, start, end []byte, fn func(k, v []byte) bool) error {
	if root == 0 {
		return nil
	}
	_, err := btreeScanNode(pa, root, start, end, 0, fn)
	return err
}

func btreeScanNode(pa pageAccess, pgid PageID, start, end []byte, depth int, fn func(k, v []byte) bool) (bool, error) {
	if depth >= maxBTreeDepth {
		return false, fmt.Errorf("%w: max btree depth exceeded during Scan", ErrCorruptedPage)
	}
	c := pa.core()
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return false, err
	}
	switch h.Flags {
	case PageFlagLeaf:
		entries, err := c.DecodeLeafPage(buf, h)
		if err != nil {
			return false, err
		}
		i := 0
		if len(start) > 0 {
			i = sort.Search(len(entries), func(j int) bool {
				return bytes.Compare(entries[j].Key, start) >= 0
			})
		}
		for ; i < len(entries); i++ {
			if len(end) > 0 && bytes.Compare(entries[i].Key, end) >= 0 {
				return false, nil
			}
			if !fn(cloneBytes(entries[i].Key), cloneBytes(entries[i].Value)) {
				return false, nil
			}
		}
		return true, nil

	case PageFlagBranch:
		entries, err := c.DecodeBranchPage(buf, h)
		if err != nil {
			return false, err
		}
		if len(entries) == 0 {
			return false, ErrCorruptedPage
		}
		startIdx := 0
		if len(start) > 0 {
			startIdx = c.BranchChildIndex(entries, start)
		}
		for i := startIdx; i < len(entries); i++ {
			if len(end) > 0 && i > 0 && bytes.Compare(entries[i].Key, end) >= 0 {
				return false, nil
			}
			cont, err := btreeScanNode(pa, entries[i].Child, start, end, depth+1, fn)
			if err != nil || !cont {
				return false, err
			}
		}
		return true, nil

	default:
		return false, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.Flags, pgid)
	}
}

func splitLeafEntries(entries []LeafEntry, pageSize, maxKeys int) [][]LeafEntry {
	bestMid := -1
	bestMaxBytes := pageSize + 1
	for mid := 1; mid < len(entries); mid++ {
		if maxKeys > 0 && (mid > maxKeys || len(entries)-mid > maxKeys) {
			continue
		}
		leftSize := leafEncodedSize(entries[:mid])
		rightSize := leafEncodedSize(entries[mid:])
		if leftSize <= pageSize && rightSize <= pageSize {
			m := leftSize
			if rightSize > m {
				m = rightSize
			}
			if m < bestMaxBytes {
				bestMaxBytes = m
				bestMid = mid
			}
		}
	}
	if bestMid != -1 {
		return [][]LeafEntry{entries[:bestMid], entries[bestMid:]}
	}

	var chunks [][]LeafEntry
	start := 0
	for start < len(entries) {
		end := start + 1
		size := pageHeaderSize + leafEntryHeaderSize + len(entries[start].Key) + len(entries[start].Value)
		for end < len(entries) {
			if maxKeys > 0 && end-start >= maxKeys {
				break
			}
			next := leafEntryHeaderSize + len(entries[end].Key) + len(entries[end].Value)
			if size+next > pageSize {
				break
			}
			size += next
			end++
		}
		chunks = append(chunks, entries[start:end])
		start = end
	}
	return chunks
}

func splitBranchEntries(entries []BranchEntry, pageSize, maxKeys int) [][]BranchEntry {
	bestMid := -1
	bestMaxBytes := pageSize + 1
	for mid := 1; mid < len(entries); mid++ {
		if maxKeys > 0 && (mid > maxKeys || len(entries)-mid > maxKeys) {
			continue
		}
		leftSize := branchEncodedSize(entries[:mid])
		rightSize := branchEncodedSize(entries[mid:])
		if leftSize <= pageSize && rightSize <= pageSize {
			m := leftSize
			if rightSize > m {
				m = rightSize
			}
			if m < bestMaxBytes {
				bestMaxBytes = m
				bestMid = mid
			}
		}
	}
	if bestMid != -1 {
		return [][]BranchEntry{entries[:bestMid], entries[bestMid:]}
	}

	var chunks [][]BranchEntry
	start := 0
	for start < len(entries) {
		end := start + 1
		size := pageHeaderSize + branchEntryHeaderSize + len(entries[start].Key)
		for end < len(entries) {
			if maxKeys > 0 && end-start >= maxKeys {
				break
			}
			next := branchEntryHeaderSize + len(entries[end].Key)
			if size+next > pageSize {
				break
			}
			size += next
			end++
		}
		chunks = append(chunks, entries[start:end])
		start = end
	}
	return chunks
}

func btreePut(pa pageAccess, root PageID, key, value []byte) (PageID, error) {
	if len(key) == 0 {
		return 0, ErrEmptyKey
	}
	if pageHeaderSize+leafEntryHeaderSize+len(key)+len(value) > pa.pageSize() {
		return 0, ErrValueTooLarge
	}
	if pageHeaderSize+2*(branchEntryHeaderSize+len(key)) > pa.pageSize() {
		return 0, ErrKeyTooLarge
	}

	c := pa.core()
	if root == 0 {
		entries := []LeafEntry{{Key: cloneBytes(key), Value: cloneBytes(value)}}
		return pa.allocPage(func(pgid PageID) ([]byte, error) {
			return encodeLeafPageWithCore(c, pa.pageSize(), pgid, pa.txid(), entries)
		})
	}

	results, err := btreePutNode(pa, root, key, value, 0)
	if err != nil {
		return 0, err
	}
	for len(results) > 1 {
		if c.BranchEncodedSize(results) <= pa.pageSize() && (pa.maxBranchKeys() == 0 || len(results) <= pa.maxBranchKeys()) {
			return pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeBranchPageWithCore(c, pa.pageSize(), pgid, pa.txid(), results)
			})
		}
		chunks := c.SplitBranchEntries(results, pa.pageSize(), pa.maxBranchKeys())
		nextLevel := make([]BranchEntry, 0, len(chunks))
		for _, chunk := range chunks {
			ch := chunk
			id, err := pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeBranchPageWithCore(c, pa.pageSize(), pgid, pa.txid(), ch)
			})
			if err != nil {
				return 0, err
			}
			nextLevel = append(nextLevel, BranchEntry{Key: cloneBytes(ch[0].Key), Child: id})
		}
		results = nextLevel
	}
	return results[0].Child, nil
}

func btreePutNode(pa pageAccess, pgid PageID, key, value []byte, depth int) ([]BranchEntry, error) {
	if depth >= maxBTreeDepth {
		return nil, fmt.Errorf("%w: max btree depth exceeded during Put", ErrCorruptedPage)
	}
	c := pa.core()
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return nil, err
	}

	switch h.Flags {
	case PageFlagLeaf:
		entries, err := c.DecodeLeafPage(buf, h)
		if err != nil {
			return nil, err
		}
		entries = c.LeafPut(entries, key, value)

		needSplit := c.LeafEncodedSize(entries) > pa.pageSize()
		if pa.maxLeafKeys() > 0 && len(entries) > pa.maxLeafKeys() {
			needSplit = true
		}

		if !needSplit {
			newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
				return encodeLeafPageWithCore(c, pa.pageSize(), id, pa.txid(), entries)
			})
			if err != nil {
				return nil, err
			}
			pa.freePage(pgid)
			return []BranchEntry{{Key: cloneBytes(entries[0].Key), Child: newID}}, nil
		}

		chunks := c.SplitLeafEntries(entries, pa.pageSize(), pa.maxLeafKeys())
		out := make([]BranchEntry, 0, len(chunks))
		for _, chunk := range chunks {
			ch := chunk
			id, err := pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeLeafPageWithCore(c, pa.pageSize(), pgid, pa.txid(), ch)
			})
			if err != nil {
				return nil, err
			}
			out = append(out, BranchEntry{Key: cloneBytes(ch[0].Key), Child: id})
		}
		pa.freePage(pgid)
		return out, nil

	case PageFlagBranch:
		entries, err := c.DecodeBranchPage(buf, h)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, ErrCorruptedPage
		}
		idx := c.BranchChildIndex(entries, key)
		childResults, err := btreePutNode(pa, entries[idx].Child, key, value, depth+1)
		if err != nil {
			return nil, err
		}

		if len(childResults) == 1 {
			entries[idx].Child = childResults[0].Child
			if len(childResults[0].Key) > 0 {
				entries[idx].Key = cloneBytes(childResults[0].Key)
			}
		} else {
			updated := make([]BranchEntry, 0, len(entries)-1+len(childResults))
			updated = append(updated, entries[:idx]...)
			updated = append(updated, childResults...)
			updated = append(updated, entries[idx+1:]...)
			entries = updated
		}

		needSplit := c.BranchEncodedSize(entries) > pa.pageSize()
		if pa.maxBranchKeys() > 0 && len(entries) > pa.maxBranchKeys() {
			needSplit = true
		}

		if !needSplit {
			newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
				return encodeBranchPageWithCore(c, pa.pageSize(), id, pa.txid(), entries)
			})
			if err != nil {
				return nil, err
			}
			pa.freePage(pgid)
			return []BranchEntry{{Key: cloneBytes(entries[0].Key), Child: newID}}, nil
		}

		chunks := c.SplitBranchEntries(entries, pa.pageSize(), pa.maxBranchKeys())
		out := make([]BranchEntry, 0, len(chunks))
		for _, chunk := range chunks {
			ch := chunk
			id, err := pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeBranchPageWithCore(c, pa.pageSize(), pgid, pa.txid(), ch)
			})
			if err != nil {
				return nil, err
			}
			out = append(out, BranchEntry{Key: cloneBytes(ch[0].Key), Child: id})
		}
		pa.freePage(pgid)
		return out, nil

	default:
		return nil, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.Flags, pgid)
	}
}

type deleteResult struct {
	newID   PageID
	newMin  []byte
	deleted bool
}

func btreeDelete(pa pageAccess, root PageID, key []byte) (PageID, bool, error) {
	if len(key) == 0 {
		return root, false, ErrEmptyKey
	}
	if root == 0 {
		return 0, false, nil
	}
	res, err := btreeDeleteNode(pa, root, key, true, 0)
	if err != nil {
		return 0, false, err
	}
	return res.newID, res.deleted, nil
}

func btreeDeleteNode(pa pageAccess, pgid PageID, key []byte, isRoot bool, depth int) (deleteResult, error) {
	if depth >= maxBTreeDepth {
		return deleteResult{}, fmt.Errorf("%w: max btree depth exceeded during Delete", ErrCorruptedPage)
	}
	c := pa.core()
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return deleteResult{}, err
	}

	switch h.Flags {
	case PageFlagLeaf:
		entries, err := c.DecodeLeafPage(buf, h)
		if err != nil {
			return deleteResult{}, err
		}
		entries, deleted := c.LeafDelete(entries, key)
		if !deleted {
			return deleteResult{newID: pgid, deleted: false}, nil
		}
		if len(entries) == 0 {
			pa.freePage(pgid)
			return deleteResult{newID: 0, deleted: true}, nil
		}
		newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeLeafPageWithCore(c, pa.pageSize(), id, pa.txid(), entries)
		})
		if err != nil {
			return deleteResult{}, err
		}
		pa.freePage(pgid)
		return deleteResult{
			newID:   newID,
			newMin:  cloneBytes(entries[0].Key),
			deleted: true,
		}, nil

	case PageFlagBranch:
		entries, err := c.DecodeBranchPage(buf, h)
		if err != nil {
			return deleteResult{}, err
		}
		if len(entries) == 0 {
			return deleteResult{}, ErrCorruptedPage
		}
		idx := c.BranchChildIndex(entries, key)
		childRes, err := btreeDeleteNode(pa, entries[idx].Child, key, false, depth+1)
		if err != nil {
			return deleteResult{}, err
		}
		if !childRes.deleted {
			return deleteResult{newID: pgid, deleted: false}, nil
		}

		if childRes.newID == 0 {
			entries = append(entries[:idx], entries[idx+1:]...)
		} else {
			entries[idx].Child = childRes.newID
			if len(childRes.newMin) > 0 {
				if len(childRes.newMin) <= len(entries[idx].Key) ||
					c.BranchEncodedSize(entries)-len(entries[idx].Key)+len(childRes.newMin) <= pa.pageSize() {
					entries[idx].Key = cloneBytes(childRes.newMin)
				}
			}
		}

		if len(entries) == 0 {
			pa.freePage(pgid)
			return deleteResult{newID: 0, deleted: true}, nil
		}
		// Collapse single-child root branch so the tree shrinks cleanly.
		if isRoot && len(entries) == 1 {
			pa.freePage(pgid)
			return deleteResult{
				newID:   entries[0].Child,
				newMin:  cloneBytes(entries[0].Key),
				deleted: true,
			}, nil
		}
		newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeBranchPageWithCore(c, pa.pageSize(), id, pa.txid(), entries)
		})
		if err != nil {
			return deleteResult{}, err
		}
		pa.freePage(pgid)
		return deleteResult{
			newID:   newID,
			newMin:  cloneBytes(entries[0].Key),
			deleted: true,
		}, nil

	default:
		return deleteResult{}, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.Flags, pgid)
	}
}

// btreeValidate checks structural B+Tree invariants: acyclicity, valid page IDs,
// valid CRC32 checksums, strictly sorted keys, and accurate branch separators.
func btreeValidate(pa pageAccess, root PageID, highWater PageID) error {
	if root == 0 {
		return nil
	}
	visited := make(map[PageID]bool)
	_, _, err := btreeValidateNode(pa, root, highWater, visited)
	return err
}

func btreeValidateNode(pa pageAccess, pgid PageID, highWater PageID, visited map[PageID]bool) (minKey, maxKey []byte, err error) {
	if pgid < 2 || pgid >= highWater {
		return nil, nil, fmt.Errorf("%w: pgid %d out of bounds [2, %d)", ErrCorruptedPage, pgid, highWater)
	}
	if visited[pgid] {
		return nil, nil, fmt.Errorf("%w: cycle detected at pgid %d", ErrCorruptedPage, pgid)
	}
	visited[pgid] = true

	c := pa.core()
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return nil, nil, err
	}

	switch h.Flags {
	case PageFlagLeaf:
		entries, err := c.DecodeLeafPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("%w: empty leaf at pgid %d", ErrCorruptedPage, pgid)
		}
		for i := 1; i < len(entries); i++ {
			if bytes.Compare(entries[i-1].Key, entries[i].Key) >= 0 {
				return nil, nil, fmt.Errorf("%w: unsorted keys in leaf %d", ErrCorruptedPage, pgid)
			}
		}
		return entries[0].Key, entries[len(entries)-1].Key, nil

	case PageFlagBranch:
		entries, err := c.DecodeBranchPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("%w: empty branch at pgid %d", ErrCorruptedPage, pgid)
		}
		var prevMax []byte
		var firstMin []byte
		for i, e := range entries {
			cMin, cMax, err := btreeValidateNode(pa, e.Child, highWater, visited)
			if err != nil {
				return nil, nil, err
			}
			if i == 0 {
				firstMin = cMin
			} else {
				if bytes.Compare(prevMax, cMin) >= 0 {
					return nil, nil, fmt.Errorf("%w: overlapping child key ranges in branch %d", ErrCorruptedPage, pgid)
				}
				if bytes.Compare(e.Key, cMin) > 0 {
					return nil, nil, fmt.Errorf("%w: invalid separator in branch %d", ErrCorruptedPage, pgid)
				}
			}
			prevMax = cMax
		}
		return firstMin, prevMax, nil

	default:
		return nil, nil, fmt.Errorf("%w: unexpected flags %d at pgid %d", ErrCorruptedPage, h.Flags, pgid)
	}
}
