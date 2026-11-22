package engine

import (
	"bytes"
	"fmt"
	"sort"
)

// pageAccess abstracts page reading and append-only COW page allocation for B+Tree operations.
type pageAccess interface {
	pageSize() int
	maxLeafKeys() int
	maxBranchKeys() int
	txid() uint64
	readPage(pgid PageID) ([]byte, pageHeader, error)
	allocPage(encode func(pgid PageID) ([]byte, error)) (PageID, error)
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func btreeGet(pa pageAccess, root PageID, key []byte) ([]byte, bool, error) {
	if root == 0 {
		return nil, false, nil
	}
	curr := root
	for {
		buf, h, err := pa.readPage(curr)
		if err != nil {
			return nil, false, err
		}
		switch h.flags {
		case PageFlagLeaf:
			entries, err := decodeLeafPage(buf, h)
			if err != nil {
				return nil, false, err
			}
			idx := sort.Search(len(entries), func(i int) bool {
				return bytes.Compare(entries[i].key, key) >= 0
			})
			if idx < len(entries) && bytes.Equal(entries[idx].key, key) {
				return cloneBytes(entries[idx].value), true, nil
			}
			return nil, false, nil

		case PageFlagBranch:
			entries, err := decodeBranchPage(buf, h)
			if err != nil {
				return nil, false, err
			}
			if len(entries) == 0 {
				return nil, false, ErrCorruptedPage
			}
			idx := branchChildIndex(entries, key)
			curr = entries[idx].child

		default:
			return nil, false, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.flags, curr)
		}
	}
}

// branchChildIndex returns the child index for key using BoltDB's convention:
// the largest index i where entries[i].key <= key, clamped to 0 if key < entries[0].key.
func branchChildIndex(entries []branchEntry, key []byte) int {
	// First index with entries[i].key > key
	upper := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].key, key) > 0
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
	_, err := btreeScanNode(pa, root, start, end, fn)
	return err
}

func btreeScanNode(pa pageAccess, pgid PageID, start, end []byte, fn func(k, v []byte) bool) (bool, error) {
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return false, err
	}
	switch h.flags {
	case PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return false, err
		}
		i := 0
		if len(start) > 0 {
			i = sort.Search(len(entries), func(j int) bool {
				return bytes.Compare(entries[j].key, start) >= 0
			})
		}
		for ; i < len(entries); i++ {
			if len(end) > 0 && bytes.Compare(entries[i].key, end) >= 0 {
				return false, nil
			}
			if !fn(cloneBytes(entries[i].key), cloneBytes(entries[i].value)) {
				return false, nil
			}
		}
		return true, nil

	case PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return false, err
		}
		startIdx := 0
		if len(start) > 0 {
			startIdx = branchChildIndex(entries, start)
		}
		for i := startIdx; i < len(entries); i++ {
			if len(end) > 0 && i > 0 && bytes.Compare(entries[i].key, end) >= 0 {
				return false, nil
			}
			cont, err := btreeScanNode(pa, entries[i].child, start, end, fn)
			if err != nil || !cont {
				return false, err
			}
		}
		return true, nil

	default:
		return false, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.flags, pgid)
	}
}

type splitResult struct {
	leftID   PageID
	leftMin  []byte
	rightID  PageID
	splitKey []byte
	didSplit bool
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

	if root == 0 {
		entries := []leafEntry{{key: cloneBytes(key), value: cloneBytes(value)}}
		return pa.allocPage(func(pgid PageID) ([]byte, error) {
			return encodeLeafPage(pa.pageSize(), pgid, pa.txid(), entries)
		})
	}

	res, err := btreePutNode(pa, root, key, value)
	if err != nil {
		return 0, err
	}
	if !res.didSplit {
		return res.leftID, nil
	}

	// Root split: allocate a new root branch pointing to left and right subtrees.
	rootEntries := []branchEntry{
		{key: cloneBytes(res.leftMin), child: res.leftID},
		{key: cloneBytes(res.splitKey), child: res.rightID},
	}
	return pa.allocPage(func(pgid PageID) ([]byte, error) {
		return encodeBranchPage(pa.pageSize(), pgid, pa.txid(), rootEntries)
	})
}

func btreePutNode(pa pageAccess, pgid PageID, key, value []byte) (splitResult, error) {
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return splitResult{}, err
	}

	switch h.flags {
	case PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return splitResult{}, err
		}
		idx := sort.Search(len(entries), func(i int) bool {
			return bytes.Compare(entries[i].key, key) >= 0
		})
		if idx < len(entries) && bytes.Equal(entries[idx].key, key) {
			entries[idx].value = cloneBytes(value)
		} else {
			entries = append(entries, leafEntry{})
			copy(entries[idx+1:], entries[idx:])
			entries[idx] = leafEntry{key: cloneBytes(key), value: cloneBytes(value)}
		}

		needSplit := leafEncodedSize(entries) > pa.pageSize()
		if pa.maxLeafKeys() > 0 && len(entries) > pa.maxLeafKeys() {
			needSplit = true
		}

		if !needSplit {
			newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
				return encodeLeafPage(pa.pageSize(), id, pa.txid(), entries)
			})
			if err != nil {
				return splitResult{}, err
			}
			return splitResult{leftID: newID, leftMin: cloneBytes(entries[0].key)}, nil
		}

		mid := len(entries) / 2
		leftEntries := entries[:mid]
		rightEntries := entries[mid:]
		leftID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeLeafPage(pa.pageSize(), id, pa.txid(), leftEntries)
		})
		if err != nil {
			return splitResult{}, err
		}
		rightID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeLeafPage(pa.pageSize(), id, pa.txid(), rightEntries)
		})
		if err != nil {
			return splitResult{}, err
		}
		return splitResult{
			leftID:   leftID,
			leftMin:  cloneBytes(leftEntries[0].key),
			rightID:  rightID,
			splitKey: cloneBytes(rightEntries[0].key),
			didSplit: true,
		}, nil

	case PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return splitResult{}, err
		}
		idx := branchChildIndex(entries, key)
		childRes, err := btreePutNode(pa, entries[idx].child, key, value)
		if err != nil {
			return splitResult{}, err
		}

		entries[idx].child = childRes.leftID
		if len(childRes.leftMin) > 0 {
			entries[idx].key = cloneBytes(childRes.leftMin)
		}
		if childRes.didSplit {
			entries = append(entries, branchEntry{})
			copy(entries[idx+2:], entries[idx+1:])
			entries[idx+1] = branchEntry{key: cloneBytes(childRes.splitKey), child: childRes.rightID}
		}

		needSplit := branchEncodedSize(entries) > pa.pageSize()
		if pa.maxBranchKeys() > 0 && len(entries) > pa.maxBranchKeys() {
			needSplit = true
		}

		if !needSplit {
			newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
				return encodeBranchPage(pa.pageSize(), id, pa.txid(), entries)
			})
			if err != nil {
				return splitResult{}, err
			}
			return splitResult{leftID: newID, leftMin: cloneBytes(entries[0].key)}, nil
		}

		mid := len(entries) / 2
		leftEntries := entries[:mid]
		rightEntries := entries[mid:]
		leftID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeBranchPage(pa.pageSize(), id, pa.txid(), leftEntries)
		})
		if err != nil {
			return splitResult{}, err
		}
		rightID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeBranchPage(pa.pageSize(), id, pa.txid(), rightEntries)
		})
		if err != nil {
			return splitResult{}, err
		}
		return splitResult{
			leftID:   leftID,
			leftMin:  cloneBytes(leftEntries[0].key),
			rightID:  rightID,
			splitKey: cloneBytes(rightEntries[0].key),
			didSplit: true,
		}, nil

	default:
		return splitResult{}, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.flags, pgid)
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
	res, err := btreeDeleteNode(pa, root, key, true)
	if err != nil {
		return 0, false, err
	}
	return res.newID, res.deleted, nil
}

func btreeDeleteNode(pa pageAccess, pgid PageID, key []byte, isRoot bool) (deleteResult, error) {
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return deleteResult{}, err
	}

	switch h.flags {
	case PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return deleteResult{}, err
		}
		idx := sort.Search(len(entries), func(i int) bool {
			return bytes.Compare(entries[i].key, key) >= 0
		})
		if idx >= len(entries) || !bytes.Equal(entries[idx].key, key) {
			return deleteResult{newID: pgid, deleted: false}, nil
		}
		entries = append(entries[:idx], entries[idx+1:]...)
		if len(entries) == 0 {
			return deleteResult{newID: 0, deleted: true}, nil
		}
		newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeLeafPage(pa.pageSize(), id, pa.txid(), entries)
		})
		if err != nil {
			return deleteResult{}, err
		}
		return deleteResult{
			newID:   newID,
			newMin:  cloneBytes(entries[0].key),
			deleted: true,
		}, nil

	case PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return deleteResult{}, err
		}
		idx := branchChildIndex(entries, key)
		childRes, err := btreeDeleteNode(pa, entries[idx].child, key, false)
		if err != nil {
			return deleteResult{}, err
		}
		if !childRes.deleted {
			return deleteResult{newID: pgid, deleted: false}, nil
		}

		if childRes.newID == 0 {
			entries = append(entries[:idx], entries[idx+1:]...)
		} else {
			entries[idx].child = childRes.newID
			if len(childRes.newMin) > 0 {
				entries[idx].key = cloneBytes(childRes.newMin)
			}
		}

		if len(entries) == 0 {
			return deleteResult{newID: 0, deleted: true}, nil
		}
		// Collapse single-child root branch so the tree shrinks cleanly.
		if isRoot && len(entries) == 1 {
			return deleteResult{
				newID:   entries[0].child,
				newMin:  cloneBytes(entries[0].key),
				deleted: true,
			}, nil
		}
		newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeBranchPage(pa.pageSize(), id, pa.txid(), entries)
		})
		if err != nil {
			return deleteResult{}, err
		}
		return deleteResult{
			newID:   newID,
			newMin:  cloneBytes(entries[0].key),
			deleted: true,
		}, nil

	default:
		return deleteResult{}, fmt.Errorf("%w: unexpected page flags %d at pgid %d", ErrCorruptedPage, h.flags, pgid)
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

	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return nil, nil, err
	}

	switch h.flags {
	case PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("%w: empty leaf at pgid %d", ErrCorruptedPage, pgid)
		}
		for i := 1; i < len(entries); i++ {
			if bytes.Compare(entries[i-1].key, entries[i].key) >= 0 {
				return nil, nil, fmt.Errorf("%w: unsorted keys in leaf %d", ErrCorruptedPage, pgid)
			}
		}
		return entries[0].key, entries[len(entries)-1].key, nil

	case PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("%w: empty branch at pgid %d", ErrCorruptedPage, pgid)
		}
		var prevMax []byte
		var firstMin []byte
		for i, e := range entries {
			cMin, cMax, err := btreeValidateNode(pa, e.child, highWater, visited)
			if err != nil {
				return nil, nil, err
			}
			if i == 0 {
				firstMin = cMin
			} else {
				if bytes.Compare(prevMax, cMin) >= 0 {
					return nil, nil, fmt.Errorf("%w: overlapping child key ranges in branch %d", ErrCorruptedPage, pgid)
				}
				if bytes.Compare(e.key, cMin) > 0 {
					return nil, nil, fmt.Errorf("%w: invalid separator in branch %d", ErrCorruptedPage, pgid)
				}
			}
			prevMax = cMax
		}
		return firstMin, prevMax, nil

	default:
		return nil, nil, fmt.Errorf("%w: unexpected flags %d at pgid %d", ErrCorruptedPage, h.flags, pgid)
	}
}
