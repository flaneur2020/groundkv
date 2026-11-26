// GroundKV Phase 1 — Verified B+Tree Node Operations & Byte-Aware Multi-Way Splitting
// Proves exact KV-map refinement for LeafGet/LeafPut/LeafDelete, BoltDB BranchChildIndex
// search invariant, and byte-aware 2-way + greedy multi-way split soundness.

include "Types.dfy"
include "PageCodec.dfy"

module GroundKVNode {
  import opened GroundKVTypes
  import opened GroundKVCodec

  // =========================================================================
  // Structural Predicates & Abstract KV-Map Semantics
  // =========================================================================

  predicate LeafSorted(entries: seq<LeafEntry>)
  {
    forall i, j | 0 <= i < j < |entries| :: BytesLt(entries[i].key, entries[j].key)
  }

  predicate BranchSorted(entries: seq<BranchEntry>)
  {
    forall i, j | 0 <= i < j < |entries| :: BytesLt(entries[i].key, entries[j].key)
  }

  function LeafMap(entries: seq<LeafEntry>): map<Bytes, Bytes>
    decreases |entries|
  {
    if |entries| == 0 then map[]
    else LeafMap(entries[1..])[entries[0].key := entries[0].value]
  }

  lemma LeafMapMembership(entries: seq<LeafEntry>, k: Bytes)
    requires LeafSorted(entries)
    ensures k in LeafMap(entries) <==> exists i | 0 <= i < |entries| :: entries[i].key == k
    decreases |entries|
  {
    if |entries| == 0 {
    } else {
      LeafMapMembership(entries[1..], k);
      if k in LeafMap(entries) {
        if k == entries[0].key {
          assert entries[0].key == k;
        } else {
          var i :| 0 <= i < |entries[1..]| && entries[1..][i].key == k;
          assert entries[i + 1].key == k;
        }
      }
      if exists i | 0 <= i < |entries| :: entries[i].key == k {
        var i :| 0 <= i < |entries| && entries[i].key == k;
        if i > 0 {
          assert entries[1..][i - 1].key == k;
        }
      }
    }
  }

  lemma LeafMapAtIndex(entries: seq<LeafEntry>, i: nat)
    requires LeafSorted(entries)
    requires i < |entries|
    ensures entries[i].key in LeafMap(entries)
    ensures LeafMap(entries)[entries[i].key] == entries[i].value
    decreases |entries|
  {
    if i == 0 {
    } else {
      assert BytesLt(entries[0].key, entries[i].key);
      BytesCompareEqIff(entries[0].key, entries[i].key);
      assert entries[0].key != entries[i].key;
      LeafMapAtIndex(entries[1..], i - 1);
    }
  }

  lemma LeafMapConcat(a: seq<LeafEntry>, b: seq<LeafEntry>)
    requires LeafSorted(a + b)
    ensures LeafMap(a + b) == map k | k in LeafMap(a).Keys + LeafMap(b).Keys ::
      if k in LeafMap(a) then LeafMap(a)[k] else LeafMap(b)[k]
    decreases |a|
  {
    if |a| == 0 {
      assert a + b == b;
    } else {
      assert (a + b)[0] == a[0];
      assert (a + b)[1..] == a[1..] + b;
      LeafMapConcat(a[1..], b);
      var lhs := LeafMap(a + b);
      var rhs := map k | k in LeafMap(a).Keys + LeafMap(b).Keys ::
        if k in LeafMap(a) then LeafMap(a)[k] else LeafMap(b)[k];
      forall k | k in lhs
        ensures k in rhs && lhs[k] == rhs[k]
      {
        if k == a[0].key {
          assert k in LeafMap(a);
          assert LeafMap(a)[k] == a[0].value;
        } else {
          assert k in LeafMap(a[1..] + b);
        }
      }
      forall k | k in rhs
        ensures k in lhs && lhs[k] == rhs[k]
      {
        if k == a[0].key {
        } else {
          assert k in LeafMap(a[1..]) || k in LeafMap(b);
        }
      }
      assert lhs == rhs;
    }
  }

  // =========================================================================
  // Verified Binary Search for Leaf & Branch Nodes
  // =========================================================================

  // Lower-bound binary search: returns smallest idx in [0..|entries|] with
  // key <= entries[idx].key (or |entries| if all keys < key).
  method LeafSearchIndex(entries: seq<LeafEntry>, key: Bytes) returns (idx: nat)
    requires LeafSorted(entries)
    ensures idx <= |entries|
    ensures forall i | 0 <= i < idx :: BytesLt(entries[i].key, key)
    ensures forall i | idx <= i < |entries| :: BytesLe(key, entries[i].key)
  {
    var lo: nat := 0;
    var hi: nat := |entries|;
    while lo < hi
      invariant lo <= hi <= |entries|
      invariant forall i | 0 <= i < lo :: BytesLt(entries[i].key, key)
      invariant forall i | hi <= i < |entries| :: BytesLe(key, entries[i].key)
    {
      var mid := lo + (hi - lo) / 2;
      var cmp := BytesCompare(entries[mid].key, key);
      BytesCompareAntisymmetric(entries[mid].key, key);
      if cmp < 0 {
        forall i | 0 <= i <= mid
          ensures BytesLt(entries[i].key, key)
        {
          if i < mid {
            BytesCompareTransitive(entries[i].key, entries[mid].key, key);
          }
        }
        lo := mid + 1;
      } else {
        forall i | mid <= i < |entries|
          ensures BytesLe(key, entries[i].key)
        {
          if mid < i {
            BytesLeLtTransitive(key, entries[mid].key, entries[i].key);
          }
        }
        hi := mid;
      }
    }
    idx := lo;
  }

  // BoltDB branchChildIndex: largest index i where entries[i].key <= key,
  // clamped to 0 if key < entries[0].key.
  method BranchChildIndex(entries: seq<BranchEntry>, key: Bytes) returns (idx: nat)
    requires |entries| >= 1
    requires BranchSorted(entries)
    ensures idx < |entries|
    ensures idx > 0 ==> BytesLe(entries[idx].key, key)
    ensures idx + 1 < |entries| ==> BytesLt(key, entries[idx + 1].key)
    ensures forall i | 0 <= i < idx :: BytesLt(entries[i].key, key)
    ensures forall i | idx < i < |entries| :: BytesLt(key, entries[i].key)
  {
    var lo: nat := 0;
    var hi: nat := |entries|;
    while lo < hi
      invariant lo <= hi <= |entries|
      invariant forall i | 0 <= i < lo :: BytesLe(entries[i].key, key)
      invariant forall i | hi <= i < |entries| :: BytesLt(key, entries[i].key)
    {
      var mid := lo + (hi - lo) / 2;
      var cmp := BytesCompare(entries[mid].key, key);
      BytesCompareAntisymmetric(entries[mid].key, key);
      if cmp > 0 {
        forall i | mid <= i < |entries|
          ensures BytesLt(key, entries[i].key)
        {
          if mid < i {
            BytesCompareTransitive(key, entries[mid].key, entries[i].key);
          }
        }
        hi := mid;
      } else {
        forall i | 0 <= i <= mid
          ensures BytesLe(entries[i].key, key)
        {
          if i < mid {
            BytesLtLeTransitive(entries[i].key, entries[mid].key, key);
          }
        }
        lo := mid + 1;
      }
    }
    if lo == 0 {
      idx := 0;
    } else {
      idx := lo - 1;
      forall i | 0 <= i < idx
        ensures BytesLt(entries[i].key, key)
      {
        BytesLtLeTransitive(entries[i].key, entries[idx].key, key);
      }
    }
  }

  // =========================================================================
  // Verified Leaf Get / Put / Delete with Exact KV-Map Refinement
  // =========================================================================

  method LeafGet(entries: seq<LeafEntry>, key: Bytes) returns (res: Option<Bytes>)
    requires LeafSorted(entries)
    ensures res == (if key in LeafMap(entries) then Some(LeafMap(entries)[key]) else None)
  {
    var idx := LeafSearchIndex(entries, key);
    if idx < |entries| && entries[idx].key == key {
      LeafMapAtIndex(entries, idx);
      res := Some(entries[idx].value);
    } else {
      assert !(key in LeafMap(entries)) by {
        LeafMapMembership(entries, key);
        forall i | 0 <= i < |entries|
          ensures entries[i].key != key
        {
          if i < idx {
            BytesCompareEqIff(entries[i].key, key);
          } else if i == idx {
          } else {
            BytesLeLtTransitive(key, entries[idx].key, entries[i].key);
            BytesCompareEqIff(key, entries[i].key);
          }
        }
      }
      res := None;
    }
  }

  method LeafPut(entries: seq<LeafEntry>, key: Bytes, value: Bytes) returns (out: seq<LeafEntry>)
    requires LeafSorted(entries)
    ensures LeafSorted(out)
    ensures |out| >= 1
    ensures LeafMap(out) == LeafMap(entries)[key := value]
    ensures forall i | 0 <= i < |out| ::
      out[i] == LeafEntry(key, value) || out[i] in entries
  {
    var idx := LeafSearchIndex(entries, key);
    if idx < |entries| && entries[idx].key == key {
      out := entries[..idx] + [LeafEntry(key, value)] + entries[idx + 1..];
      assert LeafSorted(out) by {
        forall i, j | 0 <= i < j < |out|
          ensures BytesLt(out[i].key, out[j].key)
        {
          assert out[i].key == entries[i].key;
          assert out[j].key == entries[j].key;
        }
      }
      LeafPutReplaceMapLemma(entries, idx, key, value);
    } else {
      out := entries[..idx] + [LeafEntry(key, value)] + entries[idx..];
      assert LeafSorted(out) by {
        forall i, j | 0 <= i < j < |out|
          ensures BytesLt(out[i].key, out[j].key)
        {
          if j < idx {
            assert out[i] == entries[i] && out[j] == entries[j];
          } else if j == idx {
            assert out[i] == entries[i] && out[j].key == key;
          } else if i == idx {
            assert out[i].key == key && out[j] == entries[j - 1];
            BytesCompareEqIff(key, entries[j - 1].key);
            if j - 1 == idx {
              assert entries[idx].key != key;
            } else {
              BytesLeLtTransitive(key, entries[idx].key, entries[j - 1].key);
            }
          } else if i < idx {
            assert out[i] == entries[i] && out[j] == entries[j - 1];
          } else {
            assert out[i] == entries[i - 1] && out[j] == entries[j - 1];
          }
        }
      }
      forall i | idx <= i < |entries|
        ensures BytesLt(key, entries[i].key)
      {
        BytesCompareEqIff(key, entries[i].key);
        if i == idx {
          assert entries[idx].key != key;
        } else {
          BytesLeLtTransitive(key, entries[idx].key, entries[i].key);
        }
      }
      LeafPutInsertMapLemma(entries, idx, key, value);
    }
  }

  lemma LeafPutReplaceMapLemma(entries: seq<LeafEntry>, idx: nat, key: Bytes, value: Bytes)
    requires LeafSorted(entries)
    requires idx < |entries| && entries[idx].key == key
    requires LeafSorted(entries[..idx] + [LeafEntry(key, value)] + entries[idx + 1..])
    ensures LeafMap(entries[..idx] + [LeafEntry(key, value)] + entries[idx + 1..]) ==
            LeafMap(entries)[key := value]
  {
    var out := entries[..idx] + [LeafEntry(key, value)] + entries[idx + 1..];
    var mOut := LeafMap(out);
    var mExp := LeafMap(entries)[key := value];
    forall k | k in mOut
      ensures k in mExp && mOut[k] == mExp[k]
    {
      LeafMapMembership(out, k);
      var i :| 0 <= i < |out| && out[i].key == k;
      LeafMapAtIndex(out, i);
      if i == idx {
        assert k == key && mOut[k] == value;
      } else {
        assert entries[i] == out[i];
        LeafMapAtIndex(entries, i);
        BytesCompareEqIff(entries[i].key, entries[idx].key);
        assert k != key;
      }
    }
    forall k | k in mExp
      ensures k in mOut && mOut[k] == mExp[k]
    {
      if k == key {
        LeafMapAtIndex(out, idx);
      } else {
        LeafMapMembership(entries, k);
        var i :| 0 <= i < |entries| && entries[i].key == k;
        assert i != idx;
        assert out[i] == entries[i];
        LeafMapAtIndex(entries, i);
        LeafMapAtIndex(out, i);
      }
    }
    assert mOut == mExp;
  }

  lemma LeafPutInsertMapLemma(entries: seq<LeafEntry>, idx: nat, key: Bytes, value: Bytes)
    requires LeafSorted(entries)
    requires idx <= |entries|
    requires forall i | 0 <= i < idx :: BytesLt(entries[i].key, key)
    requires forall i | idx <= i < |entries| :: BytesLt(key, entries[i].key)
    requires LeafSorted(entries[..idx] + [LeafEntry(key, value)] + entries[idx..])
    ensures LeafMap(entries[..idx] + [LeafEntry(key, value)] + entries[idx..]) ==
            LeafMap(entries)[key := value]
  {
    var out := entries[..idx] + [LeafEntry(key, value)] + entries[idx..];
    assert |out| == |entries| + 1;
    assert out[idx] == LeafEntry(key, value);
    var mOut := LeafMap(out);
    var mExp := LeafMap(entries)[key := value];
    forall k | k in mOut
      ensures k in mExp && mOut[k] == mExp[k]
    {
      LeafMapMembership(out, k);
      var i :| 0 <= i < |out| && out[i].key == k;
      LeafMapAtIndex(out, i);
      if i == idx {
        assert k == key && mOut[k] == value;
      } else if i < idx {
        assert entries[i] == out[i];
        LeafMapAtIndex(entries, i);
        BytesCompareEqIff(entries[i].key, key);
        assert k != key;
      } else {
        assert entries[i - 1] == out[i];
        LeafMapAtIndex(entries, i - 1);
        BytesCompareEqIff(key, entries[i - 1].key);
        assert k != key;
      }
    }
    forall k | k in mExp
      ensures k in mOut && mOut[k] == mExp[k]
    {
      if k == key {
        LeafMapAtIndex(out, idx);
        assert mOut[key] == value;
      } else {
        assert k in LeafMap(entries);
        LeafMapMembership(entries, k);
        var i :| 0 <= i < |entries| && entries[i].key == k;
        LeafMapAtIndex(entries, i);
        if i < idx {
          assert out[i] == entries[i];
          LeafMapAtIndex(out, i);
        } else {
          assert out[i + 1] == entries[i];
          LeafMapAtIndex(out, i + 1);
        }
      }
    }
    assert mOut == mExp;
  }

  method LeafDelete(entries: seq<LeafEntry>, key: Bytes) returns (out: seq<LeafEntry>, deleted: bool)
    requires LeafSorted(entries)
    ensures LeafSorted(out)
    ensures deleted <==> key in LeafMap(entries)
    ensures LeafMap(out) == LeafMap(entries) - {key}
    ensures !deleted ==> out == entries
    ensures forall i | 0 <= i < |out| :: out[i] in entries
  {
    var idx := LeafSearchIndex(entries, key);
    if idx >= |entries| || entries[idx].key != key {
      assert !(key in LeafMap(entries)) by {
        LeafMapMembership(entries, key);
        forall i | 0 <= i < |entries|
          ensures entries[i].key != key
        {
          if i < idx {
            BytesCompareEqIff(entries[i].key, key);
          } else if i == idx {
          } else {
            BytesLeLtTransitive(key, entries[idx].key, entries[i].key);
            BytesCompareEqIff(key, entries[i].key);
          }
        }
      }
      return entries, false;
    }
    out := entries[..idx] + entries[idx + 1..];
    deleted := true;
    LeafMapAtIndex(entries, idx);
    assert LeafSorted(out) by {
      forall i, j | 0 <= i < j < |out|
        ensures BytesLt(out[i].key, out[j].key)
      {
        var oi := if i < idx then i else i + 1;
        var oj := if j < idx then j else j + 1;
        assert oi < oj;
        assert out[i] == entries[oi] && out[j] == entries[oj];
      }
    }
    LeafDeleteMapLemma(entries, idx, key);
  }

  lemma LeafDeleteMapLemma(entries: seq<LeafEntry>, idx: nat, key: Bytes)
    requires LeafSorted(entries)
    requires idx < |entries| && entries[idx].key == key
    requires LeafSorted(entries[..idx] + entries[idx + 1..])
    ensures LeafMap(entries[..idx] + entries[idx + 1..]) == LeafMap(entries) - {key}
  {
    var out := entries[..idx] + entries[idx + 1..];
    var mOut := LeafMap(out);
    var mExp := LeafMap(entries) - {key};
    forall k | k in mOut
      ensures k in mExp && mOut[k] == mExp[k]
    {
      LeafMapMembership(out, k);
      var i :| 0 <= i < |out| && out[i].key == k;
      LeafMapAtIndex(out, i);
      var oi := if i < idx then i else i + 1;
      assert out[i] == entries[oi];
      assert oi != idx;
      if oi < idx {
        BytesCompareEqIff(entries[oi].key, entries[idx].key);
      } else {
        BytesCompareEqIff(entries[idx].key, entries[oi].key);
      }
      LeafMapAtIndex(entries, oi);
    }
    forall k | k in mExp
      ensures k in mOut && mOut[k] == mExp[k]
    {
      LeafMapMembership(entries, k);
      var oi :| 0 <= oi < |entries| && entries[oi].key == k;
      assert oi != idx;
      LeafMapAtIndex(entries, oi);
      var i := if oi < idx then oi else oi - 1;
      assert out[i] == entries[oi];
      LeafMapAtIndex(out, i);
    }
    assert mOut == mExp;
  }

  // =========================================================================
  // Verified Byte-Aware 2-Way + Greedy Multi-Way Splitting
  // Exact implementation of splitLeafEntries & splitBranchEntries from engine/btree.go
  // =========================================================================

  function FlattenLeaf(chunks: seq<seq<LeafEntry>>): seq<LeafEntry>
    decreases |chunks|
  {
    if |chunks| == 0 then []
    else chunks[0] + FlattenLeaf(chunks[1..])
  }

  lemma FlattenLeafAppend(chunks: seq<seq<LeafEntry>>, c: seq<LeafEntry>)
    ensures FlattenLeaf(chunks + [c]) == FlattenLeaf(chunks) + c
    decreases |chunks|
  {
    if |chunks| == 0 {
      assert chunks + [c] == [c];
    } else {
      assert (chunks + [c])[0] == chunks[0];
      assert (chunks + [c])[1..] == chunks[1..] + [c];
      FlattenLeafAppend(chunks[1..], c);
    }
  }

  function FlattenBranch(chunks: seq<seq<BranchEntry>>): seq<BranchEntry>
    decreases |chunks|
  {
    if |chunks| == 0 then []
    else chunks[0] + FlattenBranch(chunks[1..])
  }

  lemma FlattenBranchAppend(chunks: seq<seq<BranchEntry>>, c: seq<BranchEntry>)
    ensures FlattenBranch(chunks + [c]) == FlattenBranch(chunks) + c
    decreases |chunks|
  {
    if |chunks| == 0 {
      assert chunks + [c] == [c];
    } else {
      assert (chunks + [c])[0] == chunks[0];
      assert (chunks + [c])[1..] == chunks[1..] + [c];
      FlattenBranchAppend(chunks[1..], c);
    }
  }

  lemma LeafEntriesPayloadSizeAppend(a: seq<LeafEntry>, e: LeafEntry)
    ensures LeafEntriesPayloadSize(a + [e]) == LeafEntriesPayloadSize(a) + 8 + |e.key| + |e.value|
  {
    LeafEntriesPayloadSizeConcat(a, [e]);
  }

  lemma BranchEntriesPayloadSizeAppend(a: seq<BranchEntry>, e: BranchEntry)
    ensures BranchEntriesPayloadSize(a + [e]) == BranchEntriesPayloadSize(a) + 12 + |e.key|
  {
    BranchEntriesPayloadSizeConcat(a, [e]);
  }

  method SplitLeafEntries(entries: seq<LeafEntry>, pageSize: uint32, maxKeys: nat)
    returns (chunks: seq<seq<LeafEntry>>)
    requires |entries| >= 1
    requires forall i | 0 <= i < |entries| :: 24 + 8 + |entries[i].key| + |entries[i].value| <= pageSize as nat
    ensures |chunks| >= 1
    ensures FlattenLeaf(chunks) == entries
    ensures forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    ensures forall i | 0 <= i < |chunks| :: LeafEncodedSize(chunks[i]) <= pageSize as nat
    ensures maxKeys > 0 ==> forall i | 0 <= i < |chunks| :: |chunks[i]| <= maxKeys
    ensures LeafSorted(entries) ==> forall i | 0 <= i < |chunks| :: LeafSorted(chunks[i])
  {
    // Phase 1: try balanced 2-way split minimizing max(leftBytes, rightBytes).
    var bestMid: int := -1;
    var bestMaxBytes: nat := (pageSize as nat) + 1;
    var mid: nat := 1;
    while mid < |entries|
      invariant 1 <= mid <= |entries|
      invariant bestMid == -1 || (
        1 <= bestMid < |entries| &&
        LeafEncodedSize(entries[..bestMid]) <= pageSize as nat &&
        LeafEncodedSize(entries[bestMid..]) <= pageSize as nat &&
        (maxKeys == 0 || (bestMid <= maxKeys && |entries| - bestMid <= maxKeys))
      )
    {
      if maxKeys == 0 || (mid <= maxKeys && |entries| - mid <= maxKeys) {
        var leftSize := LeafEncodedSize(entries[..mid]);
        var rightSize := LeafEncodedSize(entries[mid..]);
        if leftSize <= pageSize as nat && rightSize <= pageSize as nat {
          var m := if rightSize > leftSize then rightSize else leftSize;
          if m < bestMaxBytes {
            bestMaxBytes := m;
            bestMid := mid;
          }
        }
      }
      mid := mid + 1;
    }

    if bestMid != -1 {
      var left := entries[..bestMid];
      var right := entries[bestMid..];
      chunks := [left, right];
      assert chunks[0] == left && chunks[1..] == [right];
      assert FlattenLeaf([right]) == right + FlattenLeaf([]);
      assert FlattenLeaf(chunks) == left + right;
      assert left + right == entries;
      return;
    }

    // Phase 2: greedy multi-way chunking fallback (handles e.g. 3-way splits on tiny pages).
    chunks := [];
    var start: nat := 0;
    while start < |entries|
      invariant start <= |entries|
      invariant FlattenLeaf(chunks) == entries[..start]
      invariant forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
      invariant forall i | 0 <= i < |chunks| :: LeafEncodedSize(chunks[i]) <= pageSize as nat
      invariant maxKeys > 0 ==> forall i | 0 <= i < |chunks| :: |chunks[i]| <= maxKeys
      invariant LeafSorted(entries) ==> forall i | 0 <= i < |chunks| :: LeafSorted(chunks[i])
    {
      var end := start + 1;
      var size: nat := 24 + 8 + |entries[start].key| + |entries[start].value|;
      assert entries[start..end] == [entries[start]];
      assert entries[start..end][0] == entries[start] && entries[start..end][1..] == [];
      assert LeafEntriesPayloadSize(entries[start..end]) == 8 + |entries[start].key| + |entries[start].value|;
      assert LeafEncodedSize(entries[start..end]) == size;

      while end < |entries|
        invariant start < end <= |entries|
        invariant size == LeafEncodedSize(entries[start..end])
        invariant size <= pageSize as nat
        invariant maxKeys > 0 ==> end - start <= maxKeys
      {
        if maxKeys > 0 && end - start >= maxKeys {
          break;
        }
        var next := 8 + |entries[end].key| + |entries[end].value|;
        if size + next > pageSize as nat {
          break;
        }
        assert entries[start..end + 1] == entries[start..end] + [entries[end]];
        LeafEntriesPayloadSizeAppend(entries[start..end], entries[end]);
        size := size + next;
        end := end + 1;
      }

      var c := entries[start..end];
      FlattenLeafAppend(chunks, c);
      assert entries[..start] + c == entries[..end];
      chunks := chunks + [c];
      start := end;
    }
    assert entries[..|entries|] == entries;
  }

  method SplitBranchEntries(entries: seq<BranchEntry>, pageSize: uint32, maxKeys: nat)
    returns (chunks: seq<seq<BranchEntry>>)
    requires |entries| >= 1
    requires forall i | 0 <= i < |entries| :: 24 + 12 + |entries[i].key| <= pageSize as nat
    ensures |chunks| >= 1
    ensures FlattenBranch(chunks) == entries
    ensures forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    ensures forall i | 0 <= i < |chunks| :: BranchEncodedSize(chunks[i]) <= pageSize as nat
    ensures maxKeys > 0 ==> forall i | 0 <= i < |chunks| :: |chunks[i]| <= maxKeys
    ensures BranchSorted(entries) ==> forall i | 0 <= i < |chunks| :: BranchSorted(chunks[i])
  {
    var bestMid: int := -1;
    var bestMaxBytes: nat := (pageSize as nat) + 1;
    var mid: nat := 1;
    while mid < |entries|
      invariant 1 <= mid <= |entries|
      invariant bestMid == -1 || (
        1 <= bestMid < |entries| &&
        BranchEncodedSize(entries[..bestMid]) <= pageSize as nat &&
        BranchEncodedSize(entries[bestMid..]) <= pageSize as nat &&
        (maxKeys == 0 || (bestMid <= maxKeys && |entries| - bestMid <= maxKeys))
      )
    {
      if maxKeys == 0 || (mid <= maxKeys && |entries| - mid <= maxKeys) {
        var leftSize := BranchEncodedSize(entries[..mid]);
        var rightSize := BranchEncodedSize(entries[mid..]);
        if leftSize <= pageSize as nat && rightSize <= pageSize as nat {
          var m := if rightSize > leftSize then rightSize else leftSize;
          if m < bestMaxBytes {
            bestMaxBytes := m;
            bestMid := mid;
          }
        }
      }
      mid := mid + 1;
    }

    if bestMid != -1 {
      var left := entries[..bestMid];
      var right := entries[bestMid..];
      chunks := [left, right];
      assert chunks[0] == left && chunks[1..] == [right];
      assert FlattenBranch([right]) == right + FlattenBranch([]);
      assert FlattenBranch(chunks) == left + right;
      assert left + right == entries;
      return;
    }

    chunks := [];
    var start: nat := 0;
    while start < |entries|
      invariant start <= |entries|
      invariant FlattenBranch(chunks) == entries[..start]
      invariant forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
      invariant forall i | 0 <= i < |chunks| :: BranchEncodedSize(chunks[i]) <= pageSize as nat
      invariant maxKeys > 0 ==> forall i | 0 <= i < |chunks| :: |chunks[i]| <= maxKeys
      invariant BranchSorted(entries) ==> forall i | 0 <= i < |chunks| :: BranchSorted(chunks[i])
    {
      var end := start + 1;
      var size: nat := 24 + 12 + |entries[start].key|;
      assert entries[start..end] == [entries[start]];
      assert entries[start..end][0] == entries[start] && entries[start..end][1..] == [];
      assert BranchEntriesPayloadSize(entries[start..end]) == 12 + |entries[start].key|;
      assert BranchEncodedSize(entries[start..end]) == size;

      while end < |entries|
        invariant start < end <= |entries|
        invariant size == BranchEncodedSize(entries[start..end])
        invariant size <= pageSize as nat
        invariant maxKeys > 0 ==> end - start <= maxKeys
      {
        if maxKeys > 0 && end - start >= maxKeys {
          break;
        }
        var next := 12 + |entries[end].key|;
        if size + next > pageSize as nat {
          break;
        }
        var sliceNext := entries[start..end + 1];
        var sliceCurr := entries[start..end];
        assert sliceNext == sliceCurr + [entries[end]] by {
          assert |sliceNext| == |sliceCurr| + 1;
          forall k | 0 <= k < |sliceNext| ensures sliceNext[k] == (sliceCurr + [entries[end]])[k] {}
        }
        BranchEntriesPayloadSizeAppend(sliceCurr, entries[end]);
        size := size + next;
        end := end + 1;
      }

      var c := entries[start..end];
      if BranchSorted(entries) {
        assert BranchSorted(c) by {
          forall a, b | 0 <= a < b < |c|
            ensures BytesLt(c[a].key, c[b].key)
          {
            assert c[a] == entries[start + a] && c[b] == entries[start + b];
          }
        }
      }
      FlattenBranchAppend(chunks, c);
      assert entries[..start] + c == entries[..end];
      chunks := chunks + [c];
      start := end;
    }
    assert entries[..|entries|] == entries;
  }
}
