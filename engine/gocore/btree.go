package gocore

import (
	"bytes"
	"sort"

	"github.com/flaneur2020/groundkv/engine/core"
)

type goBTreeNode struct{}

var _ core.BTreeNode = goBTreeNode{}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (goBTreeNode) LeafGet(entries []core.LeafEntry, key []byte) ([]byte, bool) {
	idx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) >= 0
	})
	if idx < len(entries) && bytes.Equal(entries[idx].Key, key) {
		return cloneBytes(entries[idx].Value), true
	}
	return nil, false
}

func (goBTreeNode) LeafPut(entries []core.LeafEntry, key, value []byte) []core.LeafEntry {
	idx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) >= 0
	})
	if idx < len(entries) && bytes.Equal(entries[idx].Key, key) {
		entries[idx].Value = cloneBytes(value)
		return entries
	}
	entries = append(entries, core.LeafEntry{})
	copy(entries[idx+1:], entries[idx:])
	entries[idx] = core.LeafEntry{Key: cloneBytes(key), Value: cloneBytes(value)}
	return entries
}

func (goBTreeNode) LeafDelete(entries []core.LeafEntry, key []byte) ([]core.LeafEntry, bool) {
	idx := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) >= 0
	})
	if idx >= len(entries) || !bytes.Equal(entries[idx].Key, key) {
		return entries, false
	}
	return append(entries[:idx], entries[idx+1:]...), true
}

// BranchChildIndex returns the child index for key using BoltDB's convention:
// the largest index i where entries[i].Key <= key, clamped to 0 if key < entries[0].Key.
func (goBTreeNode) BranchChildIndex(entries []core.BranchEntry, key []byte) int {
	upper := sort.Search(len(entries), func(i int) bool {
		return bytes.Compare(entries[i].Key, key) > 0
	})
	if upper == 0 {
		return 0
	}
	return upper - 1
}

// splitEntries performs balanced byte-aware 2-way splitting when possible,
// falling back to greedy multi-way packing when 2 pages cannot hold all entries.
func splitEntries[T any](entries []T, pageSize, maxKeys int, entryBytes func(T) int) [][]T {
	bestMid := -1
	bestMaxBytes := pageSize + 1
	totalBytes := core.PageHeaderSize
	for _, e := range entries {
		totalBytes += entryBytes(e)
	}
	leftSize := core.PageHeaderSize
	for mid := 1; mid < len(entries); mid++ {
		leftSize += entryBytes(entries[mid-1])
		rightSize := core.PageHeaderSize + (totalBytes - leftSize)
		if maxKeys > 0 && (mid > maxKeys || len(entries)-mid > maxKeys) {
			continue
		}
		if leftSize <= pageSize && rightSize <= pageSize {
			m := max(leftSize, rightSize)
			if m < bestMaxBytes {
				bestMaxBytes = m
				bestMid = mid
			}
		}
	}
	if bestMid != -1 {
		return [][]T{entries[:bestMid], entries[bestMid:]}
	}

	var chunks [][]T
	for start := 0; start < len(entries); {
		end := start + 1
		size := core.PageHeaderSize + entryBytes(entries[start])
		for end < len(entries) {
			if maxKeys > 0 && end-start >= maxKeys {
				break
			}
			next := entryBytes(entries[end])
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

func splitLeafEntries(entries []core.LeafEntry, pageSize, maxKeys int) [][]core.LeafEntry {
	return splitEntries(entries, pageSize, maxKeys, func(e core.LeafEntry) int {
		return core.LeafEntryHeaderSize + len(e.Key) + len(e.Value)
	})
}

func splitBranchEntries(entries []core.BranchEntry, pageSize, maxKeys int) [][]core.BranchEntry {
	return splitEntries(entries, pageSize, maxKeys, func(e core.BranchEntry) int {
		return core.BranchEntryHeaderSize + len(e.Key)
	})
}

func (goBTreeNode) SplitLeafEntries(entries []core.LeafEntry, pageSize, maxKeys int) [][]core.LeafEntry {
	return splitLeafEntries(entries, pageSize, maxKeys)
}

func (goBTreeNode) SplitBranchEntries(entries []core.BranchEntry, pageSize, maxKeys int) [][]core.BranchEntry {
	return splitBranchEntries(entries, pageSize, maxKeys)
}
