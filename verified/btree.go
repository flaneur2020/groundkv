package verified

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVNode"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

type pageAccess interface {
	pageSize() int
	maxLeafKeys() int
	maxBranchKeys() int
	txid() uint64
	readPage(pgid PageID) ([]byte, pageHeader, error)
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
	curr := root
	for depth := 0; depth < maxBTreeDepth; depth++ {
		buf, h, err := pa.readPage(curr)
		if err != nil {
			return nil, false, err
		}
		switch h.flags {
		case engine.PageFlagLeaf:
			entries, err := decodeLeafPage(buf, h)
			if err != nil {
				return nil, false, err
			}
			opt := GroundKVNode.Companion_Default___.LeafGet(toDafnyLeafEntries(entries), _dafny.SeqOfBytes(key))
			if opt.Is_Some() {
				return _dafny.ToByteArray(opt.Dtor_value().(_dafny.Sequence)), true, nil
			}
			return nil, false, nil

		case engine.PageFlagBranch:
			entries, err := decodeBranchPage(buf, h)
			if err != nil {
				return nil, false, err
			}
			if len(entries) == 0 {
				return nil, false, engine.ErrCorruptedPage
			}
			idx := branchChildIndex(entries, key)
			curr = entries[idx].child

		default:
			return nil, false, fmt.Errorf("%w: unexpected page flags %d at pgid %d", engine.ErrCorruptedPage, h.flags, curr)
		}
	}
	return nil, false, fmt.Errorf("%w: max btree depth exceeded during Get", engine.ErrCorruptedPage)
}

func branchChildIndex(entries []branchEntry, key []byte) int {
	return GroundKVNode.Companion_Default___.BranchChildIndex(
		toDafnyBranchEntries(entries),
		_dafny.SeqOfBytes(key),
	).Int()
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
		return false, fmt.Errorf("%w: max btree depth exceeded during Scan", engine.ErrCorruptedPage)
	}
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return false, err
	}
	switch h.flags {
	case engine.PageFlagLeaf:
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

	case engine.PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return false, err
		}
		if len(entries) == 0 {
			return false, engine.ErrCorruptedPage
		}
		startIdx := 0
		if len(start) > 0 {
			startIdx = branchChildIndex(entries, start)
		}
		for i := startIdx; i < len(entries); i++ {
			if len(end) > 0 && i > 0 && bytes.Compare(entries[i].key, end) >= 0 {
				return false, nil
			}
			cont, err := btreeScanNode(pa, entries[i].child, start, end, depth+1, fn)
			if err != nil || !cont {
				return false, err
			}
		}
		return true, nil

	default:
		return false, fmt.Errorf("%w: unexpected page flags %d at pgid %d", engine.ErrCorruptedPage, h.flags, pgid)
	}
}

func splitLeafEntries(entries []leafEntry, pageSize, maxKeys int) [][]leafEntry {
	seq := GroundKVNode.Companion_Default___.SplitLeafEntries(
		toDafnyLeafEntries(entries),
		uint32(pageSize),
		_dafny.IntOfInt64(int64(maxKeys)),
	)
	n := int(seq.Cardinality())
	chunks := make([][]leafEntry, n)
	for i := 0; i < n; i++ {
		chunks[i] = fromDafnyLeafEntries(seq.Select(uint32(i)).(_dafny.Sequence))
	}
	return chunks
}

func splitBranchEntries(entries []branchEntry, pageSize, maxKeys int) [][]branchEntry {
	seq := GroundKVNode.Companion_Default___.SplitBranchEntries(
		toDafnyBranchEntries(entries),
		uint32(pageSize),
		_dafny.IntOfInt64(int64(maxKeys)),
	)
	n := int(seq.Cardinality())
	chunks := make([][]branchEntry, n)
	for i := 0; i < n; i++ {
		chunks[i] = fromDafnyBranchEntries(seq.Select(uint32(i)).(_dafny.Sequence))
	}
	return chunks
}

func btreePut(pa pageAccess, root PageID, key, value []byte) (PageID, error) {
	if len(key) == 0 {
		return 0, engine.ErrEmptyKey
	}
	if pageHeaderSize+leafEntryHeaderSize+len(key)+len(value) > pa.pageSize() {
		return 0, engine.ErrValueTooLarge
	}
	if pageHeaderSize+2*(branchEntryHeaderSize+len(key)) > pa.pageSize() {
		return 0, engine.ErrKeyTooLarge
	}

	if root == 0 {
		entries := []leafEntry{{key: cloneBytes(key), value: cloneBytes(value)}}
		return pa.allocPage(func(pgid PageID) ([]byte, error) {
			return encodeLeafPage(pa.pageSize(), pgid, pa.txid(), entries)
		})
	}

	results, err := btreePutNode(pa, root, key, value, 0)
	if err != nil {
		return 0, err
	}
	for len(results) > 1 {
		if branchEncodedSize(results) <= pa.pageSize() && (pa.maxBranchKeys() == 0 || len(results) <= pa.maxBranchKeys()) {
			return pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeBranchPage(pa.pageSize(), pgid, pa.txid(), results)
			})
		}
		chunks := splitBranchEntries(results, pa.pageSize(), pa.maxBranchKeys())
		nextLevel := make([]branchEntry, 0, len(chunks))
		for _, chunk := range chunks {
			c := chunk
			id, err := pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeBranchPage(pa.pageSize(), pgid, pa.txid(), c)
			})
			if err != nil {
				return 0, err
			}
			nextLevel = append(nextLevel, branchEntry{key: cloneBytes(c[0].key), child: id})
		}
		results = nextLevel
	}
	return results[0].child, nil
}

func btreePutNode(pa pageAccess, pgid PageID, key, value []byte, depth int) ([]branchEntry, error) {
	if depth >= maxBTreeDepth {
		return nil, fmt.Errorf("%w: max btree depth exceeded during Put", engine.ErrCorruptedPage)
	}
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return nil, err
	}

	switch h.flags {
	case engine.PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return nil, err
		}
		entries = fromDafnyLeafEntries(GroundKVNode.Companion_Default___.LeafPut(
			toDafnyLeafEntries(entries),
			_dafny.SeqOfBytes(key),
			_dafny.SeqOfBytes(value),
		))

		needSplit := leafEncodedSize(entries) > pa.pageSize()
		if pa.maxLeafKeys() > 0 && len(entries) > pa.maxLeafKeys() {
			needSplit = true
		}

		if !needSplit {
			newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
				return encodeLeafPage(pa.pageSize(), id, pa.txid(), entries)
			})
			if err != nil {
				return nil, err
			}
			pa.freePage(pgid)
			return []branchEntry{{key: cloneBytes(entries[0].key), child: newID}}, nil
		}

		chunks := splitLeafEntries(entries, pa.pageSize(), pa.maxLeafKeys())
		out := make([]branchEntry, 0, len(chunks))
		for _, chunk := range chunks {
			c := chunk
			id, err := pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeLeafPage(pa.pageSize(), pgid, pa.txid(), c)
			})
			if err != nil {
				return nil, err
			}
			out = append(out, branchEntry{key: cloneBytes(c[0].key), child: id})
		}
		pa.freePage(pgid)
		return out, nil

	case engine.PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, engine.ErrCorruptedPage
		}
		idx := branchChildIndex(entries, key)
		childResults, err := btreePutNode(pa, entries[idx].child, key, value, depth+1)
		if err != nil {
			return nil, err
		}

		if len(childResults) == 1 {
			entries[idx].child = childResults[0].child
			if len(childResults[0].key) > 0 {
				entries[idx].key = cloneBytes(childResults[0].key)
			}
		} else {
			updated := make([]branchEntry, 0, len(entries)-1+len(childResults))
			updated = append(updated, entries[:idx]...)
			updated = append(updated, childResults...)
			updated = append(updated, entries[idx+1:]...)
			entries = updated
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
				return nil, err
			}
			pa.freePage(pgid)
			return []branchEntry{{key: cloneBytes(entries[0].key), child: newID}}, nil
		}

		chunks := splitBranchEntries(entries, pa.pageSize(), pa.maxBranchKeys())
		out := make([]branchEntry, 0, len(chunks))
		for _, chunk := range chunks {
			c := chunk
			id, err := pa.allocPage(func(pgid PageID) ([]byte, error) {
				return encodeBranchPage(pa.pageSize(), pgid, pa.txid(), c)
			})
			if err != nil {
				return nil, err
			}
			out = append(out, branchEntry{key: cloneBytes(c[0].key), child: id})
		}
		pa.freePage(pgid)
		return out, nil

	default:
		return nil, fmt.Errorf("%w: unexpected page flags %d at pgid %d", engine.ErrCorruptedPage, h.flags, pgid)
	}
}

type deleteResult struct {
	newID   PageID
	newMin  []byte
	deleted bool
}

func btreeDelete(pa pageAccess, root PageID, key []byte) (PageID, bool, error) {
	if len(key) == 0 {
		return root, false, engine.ErrEmptyKey
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
		return deleteResult{}, fmt.Errorf("%w: max btree depth exceeded during Delete", engine.ErrCorruptedPage)
	}
	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return deleteResult{}, err
	}

	switch h.flags {
	case engine.PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return deleteResult{}, err
		}
		updatedSeq, deleted := GroundKVNode.Companion_Default___.LeafDelete(
			toDafnyLeafEntries(entries),
			_dafny.SeqOfBytes(key),
		)
		if !deleted {
			return deleteResult{newID: pgid, deleted: false}, nil
		}
		entries = fromDafnyLeafEntries(updatedSeq)
		if len(entries) == 0 {
			pa.freePage(pgid)
			return deleteResult{newID: 0, deleted: true}, nil
		}
		newID, err := pa.allocPage(func(id PageID) ([]byte, error) {
			return encodeLeafPage(pa.pageSize(), id, pa.txid(), entries)
		})
		if err != nil {
			return deleteResult{}, err
		}
		pa.freePage(pgid)
		return deleteResult{
			newID:   newID,
			newMin:  cloneBytes(entries[0].key),
			deleted: true,
		}, nil

	case engine.PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return deleteResult{}, err
		}
		if len(entries) == 0 {
			return deleteResult{}, engine.ErrCorruptedPage
		}
		idx := branchChildIndex(entries, key)
		childRes, err := btreeDeleteNode(pa, entries[idx].child, key, false, depth+1)
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
				if len(childRes.newMin) <= len(entries[idx].key) ||
					branchEncodedSize(entries)-len(entries[idx].key)+len(childRes.newMin) <= pa.pageSize() {
					entries[idx].key = cloneBytes(childRes.newMin)
				}
			}
		}

		if len(entries) == 0 {
			pa.freePage(pgid)
			return deleteResult{newID: 0, deleted: true}, nil
		}
		if isRoot && len(entries) == 1 {
			pa.freePage(pgid)
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
		pa.freePage(pgid)
		return deleteResult{
			newID:   newID,
			newMin:  cloneBytes(entries[0].key),
			deleted: true,
		}, nil

	default:
		return deleteResult{}, fmt.Errorf("%w: unexpected page flags %d at pgid %d", engine.ErrCorruptedPage, h.flags, pgid)
	}
}

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
		return nil, nil, fmt.Errorf("%w: pgid %d out of bounds [2, %d)", engine.ErrCorruptedPage, pgid, highWater)
	}
	if visited[pgid] {
		return nil, nil, fmt.Errorf("%w: cycle detected at pgid %d", engine.ErrCorruptedPage, pgid)
	}
	visited[pgid] = true

	buf, h, err := pa.readPage(pgid)
	if err != nil {
		return nil, nil, err
	}

	switch h.flags {
	case engine.PageFlagLeaf:
		entries, err := decodeLeafPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("%w: empty leaf at pgid %d", engine.ErrCorruptedPage, pgid)
		}
		for i := 1; i < len(entries); i++ {
			if bytes.Compare(entries[i-1].key, entries[i].key) >= 0 {
				return nil, nil, fmt.Errorf("%w: unsorted keys in leaf %d", engine.ErrCorruptedPage, pgid)
			}
		}
		return entries[0].key, entries[len(entries)-1].key, nil

	case engine.PageFlagBranch:
		entries, err := decodeBranchPage(buf, h)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			return nil, nil, fmt.Errorf("%w: empty branch at pgid %d", engine.ErrCorruptedPage, pgid)
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
					return nil, nil, fmt.Errorf("%w: overlapping child key ranges in branch %d", engine.ErrCorruptedPage, pgid)
				}
				if bytes.Compare(e.key, cMin) > 0 {
					return nil, nil, fmt.Errorf("%w: invalid separator in branch %d", engine.ErrCorruptedPage, pgid)
				}
			}
			prevMax = cMax
		}
		return firstMin, prevMax, nil

	default:
		return nil, nil, fmt.Errorf("%w: unexpected flags %d at pgid %d", engine.ErrCorruptedPage, h.flags, pgid)
	}
}
