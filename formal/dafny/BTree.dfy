// GroundKV Phase 2 — Verified Recursive COW B+Tree over an Abstract PageStore
//
// Proves end-to-end key-value map refinement and Copy-On-Write immutability for:
//   - TreeGet:      exact lookup matching TreeInterp(store, root, height)
//   - TreeScan:     sorted range scan matching [start, end) filter of TreeInterp
//   - TreePut:      COW insert/update + byte-aware multi-way split + root promotion
//   - TreeDelete:   COW delete + empty-node pruning + single-child root collapse
//   - TreeValidate: structural integrity & acyclicity checker
//
// Every mutation preserves all previously committed pages in `store` (StoreExtends),
// guaranteeing MVCC snapshot isolation for concurrent readers.

include "Types.dfy"
include "PageCodec.dfy"
include "BTreeNode.dfy"

module GroundKVBTree {
  import opened GroundKVTypes
  import opened GroundKVCodec
  import opened GroundKVNode

  datatype Page =
    | LeafPage(leafEntries: seq<LeafEntry>)
    | BranchPage(branchEntries: seq<BranchEntry>)

  type PageStore = map<PageID, Page>

  // =========================================================================
  // Map Union & Range Filtering Helpers
  // =========================================================================

  function MapUnion(a: map<Bytes, Bytes>, b: map<Bytes, Bytes>): map<Bytes, Bytes>
  {
    map k | k in a.Keys + b.Keys :: if k in a then a[k] else b[k]
  }

  function InBounds(k: Bytes, lo: Option<Bytes>, hi: Option<Bytes>): bool
  {
    (lo.None? || BytesLe(lo.value, k)) &&
    (hi.None? || BytesLt(k, hi.value))
  }

  function InScanRange(k: Bytes, start: Bytes, end: Bytes): bool
  {
    (|start| == 0 || BytesLe(start, k)) &&
    (|end| == 0 || BytesLt(k, end))
  }

  function FilterRange(m: map<Bytes, Bytes>, start: Bytes, end: Bytes): map<Bytes, Bytes>
  {
    map k | k in m && InScanRange(k, start, end) :: m[k]
  }

  lemma FilterRangeUnion(a: map<Bytes, Bytes>, b: map<Bytes, Bytes>, start: Bytes, end: Bytes)
    ensures FilterRange(MapUnion(a, b), start, end) ==
            MapUnion(FilterRange(a, start, end), FilterRange(b, start, end))
  {
    var lhs := FilterRange(MapUnion(a, b), start, end);
    var rhs := MapUnion(FilterRange(a, start, end), FilterRange(b, start, end));
    forall k | k in lhs
      ensures k in rhs && lhs[k] == rhs[k]
    {}
    forall k | k in rhs
      ensures k in lhs && lhs[k] == rhs[k]
    {}
    assert lhs == rhs;
  }

  function ChildLo(es: seq<BranchEntry>, i: nat, lo: Option<Bytes>): Option<Bytes>
    requires i < |es|
  {
    if i == 0 then lo else Some(es[i].key)
  }

  function ChildHi(es: seq<BranchEntry>, i: nat, hi: Option<Bytes>): Option<Bytes>
    requires i < |es|
  {
    if i + 1 < |es| then Some(es[i + 1].key) else hi
  }

  // =========================================================================
  // Inductive B+Tree Structural Invariant & Abstract KV-Map Interpretation
  // =========================================================================

  predicate StoreExtends(oldStore: PageStore, newStore: PageStore)
  {
    forall p | p in oldStore :: p in newStore && newStore[p] == oldStore[p]
  }

  predicate StoreBoundedBy(store: PageStore, nextID: PageID)
  {
    forall p | p in store :: 2 <= p < nextID
  }

  predicate ValidPageEntries(p: Page, pageSize: uint32)
  {
    match p
      case LeafPage(es) =>
        forall i | 0 <= i < |es| ::
          24 + 8 + |es[i].key| + |es[i].value| <= pageSize as nat &&
          24 + 12 + |es[i].key| <= pageSize as nat
      case BranchPage(es) =>
        forall i | 0 <= i < |es| ::
          24 + 12 + |es[i].key| <= pageSize as nat
  }

  predicate StoreValidPageSizes(store: PageStore, pageSize: uint32)
  {
    forall p | p in store :: ValidPageEntries(store[p], pageSize)
  }

  predicate ValidTree(store: PageStore, pgid: PageID, height: nat, lo: Option<Bytes>, hi: Option<Bytes>)
    decreases height, 1
  {
    pgid in store &&
    match store[pgid]
      case LeafPage(es) =>
        height == 0 &&
        |es| >= 1 &&
        LeafSorted(es) &&
        (forall i | 0 <= i < |es| :: InBounds(es[i].key, lo, hi))
      case BranchPage(es) =>
        height > 0 &&
        |es| >= 1 &&
        BranchSorted(es) &&
        (forall i | 0 <= i < |es| :: InBounds(es[i].key, lo, hi)) &&
        (forall i | 0 <= i < |es| ::
          ValidTree(store, es[i].child, height - 1, ChildLo(es, i, lo), ChildHi(es, i, hi)))
  }

  function BranchInterp(store: PageStore, es: seq<BranchEntry>, childHeight: nat): map<Bytes, Bytes>
    decreases childHeight, |es| + 1
  {
    if |es| == 0 then map[]
    else MapUnion(
      TreeInterp(store, es[0].child, childHeight),
      BranchInterp(store, es[1..], childHeight)
    )
  }

  function TreeInterp(store: PageStore, pgid: PageID, height: nat): map<Bytes, Bytes>
    decreases height, 0
  {
    if pgid !in store then map[]
    else match store[pgid]
      case LeafPage(es) => LeafMap(es)
      case BranchPage(es) =>
        if height == 0 then map[]
        else BranchInterp(store, es, height - 1)
  }

  // =========================================================================
  // Fundamental Preservation & Disjointness Lemmas
  // =========================================================================

  lemma ValidTreePreserved(oldStore: PageStore, newStore: PageStore, pgid: PageID, height: nat, lo: Option<Bytes>, hi: Option<Bytes>)
    requires StoreExtends(oldStore, newStore)
    requires ValidTree(oldStore, pgid, height, lo, hi)
    ensures ValidTree(newStore, pgid, height, lo, hi)
    ensures TreeInterp(newStore, pgid, height) == TreeInterp(oldStore, pgid, height)
    decreases height
  {
    match oldStore[pgid]
      case LeafPage(es) =>
      case BranchPage(es) =>
        forall i | 0 <= i < |es|
          ensures ValidTree(newStore, es[i].child, height - 1, ChildLo(es, i, lo), ChildHi(es, i, hi))
          ensures TreeInterp(newStore, es[i].child, height - 1) == TreeInterp(oldStore, es[i].child, height - 1)
        {
          ValidTreePreserved(oldStore, newStore, es[i].child, height - 1, ChildLo(es, i, lo), ChildHi(es, i, hi));
        }
        BranchInterpPreserved(oldStore, newStore, es, height - 1);
  }

  lemma BranchInterpPreserved(oldStore: PageStore, newStore: PageStore, es: seq<BranchEntry>, childHeight: nat)
    requires forall i | 0 <= i < |es| :: TreeInterp(newStore, es[i].child, childHeight) == TreeInterp(oldStore, es[i].child, childHeight)
    ensures BranchInterp(newStore, es, childHeight) == BranchInterp(oldStore, es, childHeight)
    decreases |es|
  {
    if |es| > 0 {
      BranchInterpPreserved(oldStore, newStore, es[1..], childHeight);
    }
  }

  lemma InBoundsWiden(k: Bytes, lo1: Option<Bytes>, hi1: Option<Bytes>, lo2: Option<Bytes>, hi2: Option<Bytes>)
    requires InBounds(k, lo1, hi1)
    requires lo2.None? || (lo1.Some? && BytesLe(lo2.value, lo1.value))
    requires hi2.None? || (hi1.Some? && BytesLe(hi1.value, hi2.value))
    ensures InBounds(k, lo2, hi2)
  {
    if lo2.Some? {
      BytesLeTransitive(lo2.value, lo1.value, k);
    }
    if hi2.Some? {
      BytesLtLeTransitive(k, hi1.value, hi2.value);
    }
  }

  lemma MapUnionEmptyRight(m: map<Bytes, Bytes>)
    ensures MapUnion(m, map[]) == m
  {
    var u := MapUnion(m, map[]);
    forall k | k in u ensures k in m && u[k] == m[k] {}
    forall k | k in m ensures k in u && u[k] == m[k] {}
    assert u == m;
  }

  lemma {:induction false} ValidTreeWiden(store: PageStore, pgid: PageID, height: nat, lo1: Option<Bytes>, hi1: Option<Bytes>, lo2: Option<Bytes>, hi2: Option<Bytes>)
    requires ValidTree(store, pgid, height, lo1, hi1)
    requires lo2.None? || (lo1.Some? && BytesLe(lo2.value, lo1.value))
    requires hi2.None? || (hi1.Some? && BytesLe(hi1.value, hi2.value))
    ensures ValidTree(store, pgid, height, lo2, hi2)
    decreases height, 1
  {
    match store[pgid]
      case LeafPage(es) =>
        forall i | 0 <= i < |es|
          ensures InBounds(es[i].key, lo2, hi2)
        {
          InBoundsWiden(es[i].key, lo1, hi1, lo2, hi2);
        }
      case BranchPage(es) =>
        forall i | 0 <= i < |es|
          ensures InBounds(es[i].key, lo2, hi2)
        {
          InBoundsWiden(es[i].key, lo1, hi1, lo2, hi2);
        }
        ValidTreeWidenChild(store, es, height - 1, lo1, hi1, lo2, hi2);
  }

  lemma {:induction false} ValidTreeWidenChild(
    store: PageStore,
    es: seq<BranchEntry>,
    childHeight: nat,
    lo1: Option<Bytes>,
    hi1: Option<Bytes>,
    lo2: Option<Bytes>,
    hi2: Option<Bytes>
  )
    requires |es| >= 1
    requires forall i | 0 <= i < |es| ::
      ValidTree(store, es[i].child, childHeight, ChildLo(es, i, lo1), ChildHi(es, i, hi1))
    requires lo2.None? || (lo1.Some? && BytesLe(lo2.value, lo1.value))
    requires hi2.None? || (hi1.Some? && BytesLe(hi1.value, hi2.value))
    ensures forall i | 0 <= i < |es| ::
      ValidTree(store, es[i].child, childHeight, ChildLo(es, i, lo2), ChildHi(es, i, hi2))
    decreases childHeight + 1, 0
  {
    if |es| == 1 {
      ValidTreeWiden(store, es[0].child, childHeight, lo1, hi1, lo2, hi2);
    } else {
      BytesCompareReflexive(es[1].key);
      ValidTreeWiden(store, es[0].child, childHeight, lo1, Some(es[1].key), lo2, Some(es[1].key));
      var last := |es| - 1;
      BytesCompareReflexive(es[last].key);
      ValidTreeWiden(store, es[last].child, childHeight, Some(es[last].key), hi1, Some(es[last].key), hi2);
    }
  }

  lemma BranchInterpMember(store: PageStore, es: seq<BranchEntry>, childHeight: nat, k: Bytes)
    ensures k in BranchInterp(store, es, childHeight) <==>
            exists i | 0 <= i < |es| :: k in TreeInterp(store, es[i].child, childHeight)
    decreases |es|
  {
    if |es| == 0 {
    } else {
      BranchInterpMember(store, es[1..], childHeight, k);
      if k in BranchInterp(store, es, childHeight) {
        if k in TreeInterp(store, es[0].child, childHeight) {
          assert 0 < |es| && k in TreeInterp(store, es[0].child, childHeight);
        } else {
          var j :| 0 <= j < |es[1..]| && k in TreeInterp(store, es[1..][j].child, childHeight);
          assert es[j + 1] == es[1..][j];
        }
      }
      if exists i | 0 <= i < |es| :: k in TreeInterp(store, es[i].child, childHeight) {
        var i :| 0 <= i < |es| && k in TreeInterp(store, es[i].child, childHeight);
        if i > 0 {
          assert es[1..][i - 1] == es[i];
        }
      }
    }
  }

  lemma TreeInterpInBounds(store: PageStore, pgid: PageID, height: nat, lo: Option<Bytes>, hi: Option<Bytes>, k: Bytes)
    requires ValidTree(store, pgid, height, lo, hi)
    requires k in TreeInterp(store, pgid, height)
    ensures InBounds(k, lo, hi)
    decreases height
  {
    match store[pgid]
      case LeafPage(es) =>
        LeafMapMembership(es, k);
        var i :| 0 <= i < |es| && es[i].key == k;
        assert InBounds(es[i].key, lo, hi);
      case BranchPage(es) =>
        BranchInterpMember(store, es, height - 1, k);
        var idx :| 0 <= idx < |es| && k in TreeInterp(store, es[idx].child, height - 1);
        TreeInterpInBounds(store, es[idx].child, height - 1, ChildLo(es, idx, lo), ChildHi(es, idx, hi), k);
        if idx == 0 {
          if hi.Some? && |es| > 1 {
            BytesCompareTransitive(k, es[1].key, hi.value);
          }
        } else {
          if lo.Some? {
            BytesLeTransitive(lo.value, es[idx].key, k);
          }
          if hi.Some? && idx + 1 < |es| {
            BytesCompareTransitive(k, es[idx + 1].key, hi.value);
          }
        }
  }

  lemma BranchInterpConcat(store: PageStore, a: seq<BranchEntry>, b: seq<BranchEntry>, childHeight: nat)
    ensures BranchInterp(store, a + b, childHeight) ==
            MapUnion(BranchInterp(store, a, childHeight), BranchInterp(store, b, childHeight))
    decreases |a|
  {
    if |a| == 0 {
      assert a + b == b;
    } else {
      assert (a + b)[0] == a[0];
      assert (a + b)[1..] == a[1..] + b;
      BranchInterpConcat(store, a[1..], b, childHeight);
    }
  }

  lemma BranchChildrenDisjoint(
    store: PageStore,
    es: seq<BranchEntry>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>,
    i: nat,
    j: nat,
    k: Bytes
  )
    requires BranchSorted(es)
    requires forall idx | 0 <= idx < |es| ::
      ValidTree(store, es[idx].child, childHeight, ChildLo(es, idx, lo), ChildHi(es, idx, hi))
    requires 0 <= i < j < |es|
    requires k in TreeInterp(store, es[i].child, childHeight)
    ensures k !in TreeInterp(store, es[j].child, childHeight)
  {
    TreeInterpInBounds(store, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi), k);
    assert BytesLt(k, es[i + 1].key);
    if k in TreeInterp(store, es[j].child, childHeight) {
      TreeInterpInBounds(store, es[j].child, childHeight, ChildLo(es, j, lo), ChildHi(es, j, hi), k);
      assert BytesLe(es[j].key, k);
      if i + 1 == j {
        BytesLeLtTransitive(es[j].key, k, es[j].key);
        BytesCompareReflexive(es[j].key);
        assert false;
      } else {
        BytesCompareTransitive(k, es[i + 1].key, es[j].key);
        BytesLeLtTransitive(es[j].key, k, es[j].key);
        BytesCompareReflexive(es[j].key);
        assert false;
      }
    }
  }

  lemma BranchInterpValueAt(
    store: PageStore,
    es: seq<BranchEntry>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>,
    idx: nat,
    k: Bytes
  )
    requires BranchSorted(es)
    requires forall i | 0 <= i < |es| ::
      ValidTree(store, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi))
    requires idx < |es|
    requires k in TreeInterp(store, es[idx].child, childHeight)
    ensures k in BranchInterp(store, es, childHeight)
    ensures BranchInterp(store, es, childHeight)[k] == TreeInterp(store, es[idx].child, childHeight)[k]
    decreases |es|
  {
    if idx == 0 {
    } else {
      if k in TreeInterp(store, es[0].child, childHeight) {
        BranchChildrenDisjoint(store, es, childHeight, lo, hi, 0, idx, k);
        assert false;
      }
      forall j | 0 <= j < |es[1..]|
        ensures ValidTree(store, es[1..][j].child, childHeight, ChildLo(es[1..], j, Some(es[1].key)), ChildHi(es[1..], j, hi))
      {
        assert es[1..][j] == es[j + 1];
        assert ChildLo(es[1..], j, Some(es[1].key)) == ChildLo(es, j + 1, lo);
        assert ChildHi(es[1..], j, hi) == ChildHi(es, j + 1, hi);
      }
      BranchInterpValueAt(store, es[1..], childHeight, Some(es[1].key), hi, idx - 1, k);
    }
  }

  lemma BranchRouteKeyUnique(
    store: PageStore,
    es: seq<BranchEntry>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>,
    idx: nat,
    k: Bytes
  )
    requires |es| >= 1
    requires BranchSorted(es)
    requires forall i | 0 <= i < |es| ::
      ValidTree(store, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi))
    requires idx < |es|
    requires idx > 0 ==> BytesLe(es[idx].key, k)
    requires idx + 1 < |es| ==> BytesLt(k, es[idx + 1].key)
    requires forall i | 0 <= i < idx :: BytesLt(es[i].key, k)
    requires forall i | idx < i < |es| :: BytesLt(k, es[i].key)
    ensures forall j | 0 <= j < |es| && j != idx :: k !in TreeInterp(store, es[j].child, childHeight)
    ensures k in BranchInterp(store, es, childHeight) <==> k in TreeInterp(store, es[idx].child, childHeight)
    ensures k in TreeInterp(store, es[idx].child, childHeight) ==>
      BranchInterp(store, es, childHeight)[k] == TreeInterp(store, es[idx].child, childHeight)[k]
  {
    forall j | 0 <= j < |es| && j != idx
      ensures k !in TreeInterp(store, es[j].child, childHeight)
    {
      if k in TreeInterp(store, es[j].child, childHeight) {
        TreeInterpInBounds(store, es[j].child, childHeight, ChildLo(es, j, lo), ChildHi(es, j, hi), k);
        if j < idx {
          assert BytesLt(k, es[j + 1].key);
          if j + 1 == idx {
            BytesLeLtTransitive(es[idx].key, k, es[idx].key);
            BytesCompareReflexive(es[idx].key);
            assert false;
          } else {
            BytesCompareTransitive(k, es[j + 1].key, es[idx].key);
            BytesLeLtTransitive(es[idx].key, k, es[idx].key);
            BytesCompareReflexive(es[idx].key);
            assert false;
          }
        } else {
          assert BytesLe(es[j].key, k);
          assert BytesLt(k, es[j].key);
          BytesLeLtTransitive(es[j].key, k, es[j].key);
          BytesCompareReflexive(es[j].key);
          assert false;
        }
      }
    }
    BranchInterpMember(store, es, childHeight, k);
    if k in TreeInterp(store, es[idx].child, childHeight) {
      BranchInterpValueAt(store, es, childHeight, lo, hi, idx, k);
    }
  }

  lemma TreeNonEmptyKey(store: PageStore, pgid: PageID, height: nat, lo: Option<Bytes>, hi: Option<Bytes>)
    requires ValidTree(store, pgid, height, lo, hi)
    ensures exists k :: k in TreeInterp(store, pgid, height)
    decreases height
  {
    match store[pgid]
      case LeafPage(es) =>
        LeafMapAtIndex(es, 0);
        assert es[0].key in TreeInterp(store, pgid, height);
      case BranchPage(es) =>
        TreeNonEmptyKey(store, es[0].child, height - 1, ChildLo(es, 0, lo), ChildHi(es, 0, hi));
        var k :| k in TreeInterp(store, es[0].child, height - 1);
        assert k in TreeInterp(store, pgid, height);
  }

  // =========================================================================
  // Verified TreeGet — Exact End-to-End KV Lookup Refinement
  // =========================================================================

  method TreeGet(
    store: PageStore,
    pgid: PageID,
    ghost height: nat,
    ghost lo: Option<Bytes>,
    ghost hi: Option<Bytes>,
    key: Bytes
  ) returns (res: Option<Bytes>)
    requires ValidTree(store, pgid, height, lo, hi)
    ensures res == (if key in TreeInterp(store, pgid, height) then Some(TreeInterp(store, pgid, height)[key]) else None)
    decreases height
  {
    match store[pgid]
      case LeafPage(es) =>
        res := LeafGet(es, key);
      case BranchPage(es) =>
        var idx := BranchChildIndex(es, key);
        BranchRouteKeyUnique(store, es, height - 1, lo, hi, idx, key);
        res := TreeGet(store, es[idx].child, height - 1, ChildLo(es, idx, lo), ChildHi(es, idx, hi), key);
  }

  // =========================================================================
  // Verified TreeScan — Sorted Range Scan Refinement
  // =========================================================================

  method LeafScan(entries: seq<LeafEntry>, start: Bytes, end: Bytes) returns (out: seq<LeafEntry>)
    requires LeafSorted(entries)
    ensures LeafSorted(out)
    ensures LeafMap(out) == FilterRange(LeafMap(entries), start, end)
    ensures forall i | 0 <= i < |out| :: InScanRange(out[i].key, start, end)
    ensures forall i | 0 <= i < |out| :: out[i] in entries
  {
    out := [];
    var i: nat := 0;
    while i < |entries|
      invariant i <= |entries|
      invariant LeafSorted(out)
      invariant forall j | 0 <= j < |out| :: InScanRange(out[j].key, start, end)
      invariant forall j | 0 <= j < |out| :: out[j] in entries[..i]
      invariant forall j | 0 <= j < |out| :: i < |entries| ==> BytesLt(out[j].key, entries[i].key)
      invariant LeafMap(out) == FilterRange(LeafMap(entries[..i]), start, end)
    {
      var e := entries[i];
      assert entries[..i + 1] == entries[..i] + [e];
      LeafMapConcat(entries[..i], [e]);
      if InScanRange(e.key, start, end) {
        var nextOut := out + [e];
        assert LeafSorted(nextOut) by {
          forall a, b | 0 <= a < b < |nextOut|
            ensures BytesLt(nextOut[a].key, nextOut[b].key)
          {
            if b >= |out| {
              assert nextOut[b] == e;
            }
          }
        }
        LeafMapConcat(out, [e]);
        out := nextOut;
      }
      if i + 1 < |entries| {
        forall j | 0 <= j < |out|
          ensures BytesLt(out[j].key, entries[i + 1].key)
        {
          assert out[j] in entries[..i + 1];
          var idx :| 0 <= idx < i + 1 && entries[idx] == out[j];
          assert BytesLt(entries[idx].key, entries[i + 1].key);
        }
      }
      i := i + 1;
    }
    assert entries[..|entries|] == entries;
  }

  method TreeScan(
    store: PageStore,
    pgid: PageID,
    ghost height: nat,
    ghost lo: Option<Bytes>,
    ghost hi: Option<Bytes>,
    start: Bytes,
    end: Bytes
  ) returns (out: seq<LeafEntry>)
    requires ValidTree(store, pgid, height, lo, hi)
    ensures LeafSorted(out)
    ensures forall i | 0 <= i < |out| :: InBounds(out[i].key, lo, hi)
    ensures forall i | 0 <= i < |out| :: InScanRange(out[i].key, start, end)
    ensures LeafMap(out) == FilterRange(TreeInterp(store, pgid, height), start, end)
    decreases height
  {
    match store[pgid]
      case LeafPage(es) =>
        out := LeafScan(es, start, end);
      case BranchPage(es) =>
        out := [];
        var i: nat := 0;
        while i < |es|
          invariant i <= |es|
          invariant i == 0 ==> out == []
          invariant LeafSorted(out)
          invariant forall j | 0 <= j < |out| :: InBounds(out[j].key, lo, hi)
          invariant forall j | 0 <= j < |out| :: InScanRange(out[j].key, start, end)
          invariant i < |es| ==> forall j | 0 <= j < |out| :: BytesLt(out[j].key, es[i].key)
          invariant LeafMap(out) == FilterRange(BranchInterp(store, es[..i], height - 1), start, end)
        {
          var cLo := ChildLo(es, i, lo);
          var cHi := ChildHi(es, i, hi);
          var childOut := TreeScan(store, es[i].child, height - 1, cLo, cHi, start, end);

          assert LeafSorted(out + childOut) by {
            forall a, b | 0 <= a < b < |out + childOut|
              ensures BytesLt((out + childOut)[a].key, (out + childOut)[b].key)
            {
              if a < |out| && b >= |out| {
                assert i > 0;
                assert BytesLt(out[a].key, es[i].key);
                assert BytesLe(es[i].key, childOut[b - |out|].key);
                BytesLtLeTransitive(out[a].key, es[i].key, childOut[b - |out|].key);
              }
            }
          }
          LeafMapConcat(out, childOut);
          assert es[..i + 1] == es[..i] + [es[i]];
          BranchInterpConcat(store, es[..i], [es[i]], height - 1);
          assert BranchInterp(store, [es[i]], height - 1) == TreeInterp(store, es[i].child, height - 1);
          FilterRangeUnion(
            BranchInterp(store, es[..i], height - 1),
            TreeInterp(store, es[i].child, height - 1),
            start, end
          );
          assert LeafMap(out + childOut) == FilterRange(BranchInterp(store, es[..i + 1], height - 1), start, end);

          forall j | 0 <= j < |childOut|
            ensures InBounds(childOut[j].key, lo, hi)
          {
            if i == 0 {
              if hi.Some? && |es| > 1 {
                BytesCompareTransitive(childOut[j].key, es[1].key, hi.value);
              }
            } else {
              if lo.Some? {
                BytesLeTransitive(lo.value, es[i].key, childOut[j].key);
              }
              if hi.Some? && i + 1 < |es| {
                BytesCompareTransitive(childOut[j].key, es[i + 1].key, hi.value);
              }
            }
          }

          if i + 1 < |es| {
            forall j | 0 <= j < |out|
              ensures BytesLt(out[j].key, es[i + 1].key)
            {
              BytesCompareTransitive(out[j].key, es[i].key, es[i + 1].key);
            }
          }

          out := out + childOut;
          i := i + 1;
        }
        assert es[..|es|] == es;
  }

  // =========================================================================
  // Verified TreePut — COW Allocation + Byte-Aware Multi-Way Split + Root Promotion
  // =========================================================================

  predicate ValidPutResults(
    store: PageStore,
    results: seq<BranchEntry>,
    height: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>
  )
  {
    |results| >= 1 &&
    BranchSorted(results) &&
    (forall i | 0 <= i < |results| :: InBounds(results[i].key, lo, hi)) &&
    (forall i | 0 <= i < |results| ::
      ValidTree(store, results[i].child, height, ChildLo(results, i, lo), ChildHi(results, i, hi)))
  }

  lemma LeafSortedSplit(a: seq<LeafEntry>, b: seq<LeafEntry>)
    requires LeafSorted(a + b)
    ensures LeafSorted(a)
    ensures LeafSorted(b)
  {
    forall i, j | 0 <= i < j < |a|
      ensures BytesLt(a[i].key, a[j].key)
    {
      assert (a + b)[i] == a[i];
      assert (a + b)[j] == a[j];
    }
    forall i, j | 0 <= i < j < |b|
      ensures BytesLt(b[i].key, b[j].key)
    {
      assert (a + b)[|a| + i] == b[i];
      assert (a + b)[|a| + j] == b[j];
    }
  }

  lemma LeafChunksOrderLemma(chunks: seq<seq<LeafEntry>>, lo: Option<Bytes>, hi: Option<Bytes>)
    requires forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    requires LeafSorted(FlattenLeaf(chunks))
    requires forall i | 0 <= i < |FlattenLeaf(chunks)| :: InBounds(FlattenLeaf(chunks)[i].key, lo, hi)
    ensures forall i | 0 <= i < |chunks| :: LeafSorted(chunks[i])
    ensures forall i, j | 0 <= i < |chunks| && 0 <= j < |chunks[i]| :: InBounds(chunks[i][j].key, lo, hi)
    ensures forall i, j, a, b | 0 <= i < j < |chunks| && 0 <= a < |chunks[i]| && 0 <= b < |chunks[j]| ::
      BytesLt(chunks[i][a].key, chunks[j][b].key)
    decreases |chunks|
  {
    if |chunks| == 0 {
    } else {
      var head := chunks[0];
      var tail := chunks[1..];
      var flat := FlattenLeaf(chunks);
      var flatTail := FlattenLeaf(tail);
      assert flat == head + flatTail;
      LeafSortedSplit(head, flatTail);
      forall k | 0 <= k < |head|
        ensures InBounds(head[k].key, lo, hi)
      {
        assert flat[k] == head[k];
      }
      forall k | 0 <= k < |flatTail|
        ensures InBounds(flatTail[k].key, lo, hi)
      {
        assert flat[|head| + k] == flatTail[k];
      }
      LeafChunksOrderLemma(tail, lo, hi);
      forall i, j, a, b | 0 <= i < j < |chunks| && 0 <= a < |chunks[i]| && 0 <= b < |chunks[j]|
        ensures BytesLt(chunks[i][a].key, chunks[j][b].key)
      {
        if i == 0 {
          FlattenLeafIndex(tail, j - 1, b);
          var pos :| 0 <= pos < |flatTail| && flatTail[pos] == tail[j - 1][b];
          assert flat[a] == head[a];
          assert flat[|head| + pos] == tail[j - 1][b];
        } else {
          assert chunks[i] == tail[i - 1] && chunks[j] == tail[j - 1];
        }
      }
    }
  }

  lemma FlattenLeafIndex(chunks: seq<seq<LeafEntry>>, i: nat, j: nat)
    requires i < |chunks| && j < |chunks[i]|
    ensures exists pos | 0 <= pos < |FlattenLeaf(chunks)| :: FlattenLeaf(chunks)[pos] == chunks[i][j]
    decreases |chunks|
  {
    if i == 0 {
      assert FlattenLeaf(chunks)[j] == chunks[0][j];
    } else {
      FlattenLeafIndex(chunks[1..], i - 1, j);
      var p :| 0 <= p < |FlattenLeaf(chunks[1..])| && FlattenLeaf(chunks[1..])[p] == chunks[1..][i - 1][j];
      assert FlattenLeaf(chunks)[|chunks[0]| + p] == chunks[i][j];
    }
  }

  lemma LeafChunksInterpLemma(store: PageStore, chunks: seq<seq<LeafEntry>>, results: seq<BranchEntry>)
    requires |chunks| == |results|
    requires LeafSorted(FlattenLeaf(chunks))
    requires forall i | 0 <= i < |chunks| ::
      results[i].child in store && store[results[i].child] == LeafPage(chunks[i])
    ensures BranchInterp(store, results, 0) == LeafMap(FlattenLeaf(chunks))
    decreases |chunks|
  {
    if |chunks| == 0 {
    } else {
      LeafSortedSplit(chunks[0], FlattenLeaf(chunks[1..]));
      LeafMapConcat(chunks[0], FlattenLeaf(chunks[1..]));
      LeafChunksInterpLemma(store, chunks[1..], results[1..]);
    }
  }

  lemma LeafChunksValidPutResults(
    store: PageStore,
    chunks: seq<seq<LeafEntry>>,
    results: seq<BranchEntry>,
    lo: Option<Bytes>,
    hi: Option<Bytes>
  )
    requires |chunks| == |results| && |chunks| >= 1
    requires LeafSorted(FlattenLeaf(chunks))
    requires forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    requires forall i | 0 <= i < |chunks| ::
      results[i].key == chunks[i][0].key &&
      results[i].child in store &&
      store[results[i].child] == LeafPage(chunks[i])
    requires forall i | 0 <= i < |FlattenLeaf(chunks)| :: InBounds(FlattenLeaf(chunks)[i].key, lo, hi)
    ensures ValidPutResults(store, results, 0, lo, hi)
    ensures BranchInterp(store, results, 0) == LeafMap(FlattenLeaf(chunks))
  {
    LeafChunksOrderLemma(chunks, lo, hi);
    forall i | 0 <= i < |results|
      ensures ValidTree(store, results[i].child, 0, ChildLo(results, i, lo), ChildHi(results, i, hi))
    {
      var c := chunks[i];
      forall j | 0 <= j < |c|
        ensures InBounds(c[j].key, ChildLo(results, i, lo), ChildHi(results, i, hi))
      {
        if i > 0 {
          if j == 0 {
            BytesCompareReflexive(c[0].key);
          } else {
            assert BytesLt(c[0].key, c[j].key);
          }
        }
        if i + 1 < |results| {
          assert BytesLt(c[j].key, chunks[i + 1][0].key);
        }
      }
    }
    LeafChunksInterpLemma(store, chunks, results);
  }

  function FlattenBranchOffset(chunks: seq<seq<BranchEntry>>, i: nat): nat
    requires i <= |chunks|
    decreases i
  {
    if i == 0 then 0
    else FlattenBranchOffset(chunks, i - 1) + |chunks[i - 1]|
  }

  lemma FlattenBranchOffsetShift(chunks: seq<seq<BranchEntry>>, i: nat)
    requires 1 <= i <= |chunks|
    ensures FlattenBranchOffset(chunks, i) == |chunks[0]| + FlattenBranchOffset(chunks[1..], i - 1)
    decreases i
  {
    if i > 1 {
      FlattenBranchOffsetShift(chunks, i - 1);
    }
  }

  lemma FlattenBranchEnd(chunks: seq<seq<BranchEntry>>)
    ensures FlattenBranchOffset(chunks, |chunks|) == |FlattenBranch(chunks)|
    decreases |chunks|
  {
    if |chunks| > 0 {
      FlattenBranchOffsetShift(chunks, |chunks|);
      FlattenBranchEnd(chunks[1..]);
    }
  }

  lemma FlattenBranchPos(chunks: seq<seq<BranchEntry>>, i: nat, k: nat)
    requires i < |chunks| && k < |chunks[i]|
    ensures FlattenBranchOffset(chunks, i) + k < |FlattenBranch(chunks)|
    ensures FlattenBranch(chunks)[FlattenBranchOffset(chunks, i) + k] == chunks[i][k]
    decreases |chunks|
  {
    if i > 0 {
      FlattenBranchOffsetShift(chunks, i);
      FlattenBranchPos(chunks[1..], i - 1, k);
    }
  }

  lemma FlattenBranchOffsetMono(chunks: seq<seq<BranchEntry>>, i: nat, j: nat)
    requires forall k | 0 <= k < |chunks| :: |chunks[k]| >= 1
    requires 0 <= i < j < |chunks|
    ensures FlattenBranchOffset(chunks, i) + |chunks[i]| <= FlattenBranchOffset(chunks, j)
    decreases j - i
  {
    if j > i + 1 {
      FlattenBranchOffsetMono(chunks, i, j - 1);
    }
  }

  lemma BranchChunksOrderHelper(chunks: seq<seq<BranchEntry>>)
    requires forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    requires BranchSorted(FlattenBranch(chunks))
    ensures forall i | 0 <= i < |chunks| :: BranchSorted(chunks[i])
    ensures forall i, j, a, b | 0 <= i < j < |chunks| && 0 <= a < |chunks[i]| && 0 <= b < |chunks[j]| ::
      BytesLt(chunks[i][a].key, chunks[j][b].key)
  {
    forall i | 0 <= i < |chunks|
      ensures BranchSorted(chunks[i])
    {
      forall a, b | 0 <= a < b < |chunks[i]|
        ensures BytesLt(chunks[i][a].key, chunks[i][b].key)
      {
        FlattenBranchPos(chunks, i, a);
        FlattenBranchPos(chunks, i, b);
      }
    }
    forall i, j, a, b | 0 <= i < j < |chunks| && 0 <= a < |chunks[i]| && 0 <= b < |chunks[j]|
      ensures BytesLt(chunks[i][a].key, chunks[j][b].key)
    {
      FlattenBranchPos(chunks, i, a);
      FlattenBranchPos(chunks, j, b);
      FlattenBranchOffsetMono(chunks, i, j);
    }
  }

  function ChunkLo(chunks: seq<seq<BranchEntry>>, i: nat, lo: Option<Bytes>): Option<Bytes>
    requires i < |chunks| && |chunks[i]| >= 1
  {
    if i == 0 then lo else Some(chunks[i][0].key)
  }

  function ChunkHi(chunks: seq<seq<BranchEntry>>, i: nat, hi: Option<Bytes>): Option<Bytes>
    requires i < |chunks|
    requires i + 1 < |chunks| ==> |chunks[i + 1]| >= 1
  {
    if i + 1 < |chunks| then Some(chunks[i + 1][0].key) else hi
  }

  lemma BranchChunksOrderLemma(
    store: PageStore,
    chunks: seq<seq<BranchEntry>>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>
  )
    requires forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    requires ValidPutResults(store, FlattenBranch(chunks), childHeight, lo, hi)
    ensures forall i | 0 <= i < |chunks| :: BranchSorted(chunks[i])
    ensures forall i, j, a, b | 0 <= i < j < |chunks| && 0 <= a < |chunks[i]| && 0 <= b < |chunks[j]| ::
      BytesLt(chunks[i][a].key, chunks[j][b].key)
    ensures forall i | 0 <= i < |chunks| :: InBounds(chunks[i][0].key, lo, hi)
    ensures forall i | 0 <= i < |chunks| ::
      ValidPutResults(store, chunks[i], childHeight, ChunkLo(chunks, i, lo), ChunkHi(chunks, i, hi))
  {
    var flat := FlattenBranch(chunks);
    BranchChunksOrderHelper(chunks);
    forall i | 0 <= i < |chunks|
      ensures InBounds(chunks[i][0].key, lo, hi)
    {
      FlattenBranchPos(chunks, i, 0);
      var pos := FlattenBranchOffset(chunks, i);
      assert flat[pos] == chunks[i][0];
    }
    forall i | 0 <= i < |chunks|
      ensures ValidPutResults(store, chunks[i], childHeight, ChunkLo(chunks, i, lo), ChunkHi(chunks, i, hi))
    {
      var c := chunks[i];
      var cLo := ChunkLo(chunks, i, lo);
      var cHi := ChunkHi(chunks, i, hi);
      var base := FlattenBranchOffset(chunks, i);
      forall k | 0 <= k < |c|
        ensures InBounds(c[k].key, cLo, cHi)
      {
        FlattenBranchPos(chunks, i, k);
        assert flat[base + k] == c[k];
        if i > 0 {
          if k == 0 {
            BytesCompareReflexive(c[0].key);
          } else {
            assert BytesLt(c[0].key, c[k].key);
          }
        }
        if i + 1 < |chunks| {
          assert BytesLt(c[k].key, chunks[i + 1][0].key);
        }
      }
      forall k | 0 <= k < |c|
        ensures ValidTree(store, c[k].child, childHeight, ChildLo(c, k, cLo), ChildHi(c, k, cHi))
      {
        FlattenBranchPos(chunks, i, k);
        assert flat[base + k] == c[k];
        assert ChildLo(flat, base + k, lo) == ChildLo(c, k, cLo) by {
          if k == 0 && i == 0 {
            assert base == 0;
          } else if k == 0 && i > 0 {
            assert base >= 1;
          } else {
            assert base + k >= 1;
          }
        }
        assert ChildHi(flat, base + k, hi) == ChildHi(c, k, cHi) by {
          if k + 1 < |c| {
            FlattenBranchPos(chunks, i, k + 1);
            assert flat[base + k + 1] == c[k + 1];
          } else if i + 1 < |chunks| {
            FlattenBranchPos(chunks, i + 1, 0);
            assert FlattenBranchOffset(chunks, i + 1) == base + |c|;
            assert flat[base + k + 1] == chunks[i + 1][0];
          } else {
            FlattenBranchEnd(chunks);
            assert base + k + 1 == |flat|;
          }
        }
      }
    }
  }

  lemma BranchChunksInterpLemma(store: PageStore, chunks: seq<seq<BranchEntry>>, results: seq<BranchEntry>, childHeight: nat)
    requires |chunks| == |results|
    requires forall i | 0 <= i < |chunks| ::
      results[i].child in store &&
      store[results[i].child] == BranchPage(chunks[i])
    ensures BranchInterp(store, results, childHeight + 1) == BranchInterp(store, FlattenBranch(chunks), childHeight)
    decreases |chunks|
  {
    if |chunks| == 0 {
    } else {
      BranchInterpConcat(store, chunks[0], FlattenBranch(chunks[1..]), childHeight);
      BranchChunksInterpLemma(store, chunks[1..], results[1..], childHeight);
    }
  }

  lemma MapUnionPutHelper(mLeft: map<Bytes, Bytes>, mMid: map<Bytes, Bytes>, mRight: map<Bytes, Bytes>, key: Bytes, value: Bytes)
    requires key !in mLeft && key !in mRight
    ensures MapUnion(mLeft, MapUnion(mMid[key := value], mRight)) ==
            MapUnion(mLeft, MapUnion(mMid, mRight))[key := value]
  {
    var lhs := MapUnion(mLeft, MapUnion(mMid[key := value], mRight));
    var rhs := MapUnion(mLeft, MapUnion(mMid, mRight))[key := value];
    forall k | k in lhs ensures k in rhs && lhs[k] == rhs[k] {}
    forall k | k in rhs ensures k in lhs && lhs[k] == rhs[k] {}
    assert lhs == rhs;
  }

  lemma {:vcs_split_on_every_assert} SpliceBranchValidLemma(
    oldStore: PageStore,
    newStore: PageStore,
    es: seq<BranchEntry>,
    idx: nat,
    childRes: seq<BranchEntry>,
    splicedChild: seq<BranchEntry>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>,
    key: Bytes,
    value: Bytes
  )
    requires StoreExtends(oldStore, newStore)
    requires |es| >= 1 && idx < |es|
    requires BranchSorted(es)
    requires forall i | 0 <= i < |es| :: InBounds(es[i].key, lo, hi)
    requires forall i | 0 <= i < |es| ::
      ValidTree(oldStore, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi))
    requires InBounds(key, ChildLo(es, idx, lo), ChildHi(es, idx, hi))
    requires forall j | 0 <= j < |es| && j != idx :: key !in TreeInterp(oldStore, es[j].child, childHeight)
    requires ValidPutResults(newStore, childRes, childHeight, ChildLo(es, idx, lo), ChildHi(es, idx, hi))
    requires BranchInterp(newStore, childRes, childHeight) == TreeInterp(oldStore, es[idx].child, childHeight)[key := value]
    requires splicedChild == [if idx == 0 then childRes[0] else BranchEntry(es[idx].key, childRes[0].child)] + childRes[1..]
    ensures var updated := es[..idx] + splicedChild + es[idx + 1..];
            ValidPutResults(newStore, updated, childHeight, lo, hi) &&
            BranchInterp(newStore, updated, childHeight) == BranchInterp(oldStore, es, childHeight)[key := value]
  {
    var cLo := ChildLo(es, idx, lo);
    var cHi := ChildHi(es, idx, hi);
    var updated := es[..idx] + splicedChild + es[idx + 1..];
    var m := |splicedChild|;

    PreserveAllBranchChildren(oldStore, newStore, es, childHeight, lo, hi);

    // Prove every entry key in updated is in (lo, hi)
    forall i | 0 <= i < |updated|
      ensures InBounds(updated[i].key, lo, hi)
    {
      if i < idx {
        assert updated[i] == es[i];
      } else if i < idx + m {
        if i == idx && idx > 0 {
          assert updated[i].key == es[idx].key;
        } else {
          var k := childRes[i - idx].key;
          assert InBounds(k, cLo, cHi);
          if lo.Some? && idx > 0 {
            BytesLeTransitive(lo.value, es[idx].key, k);
          }
          if hi.Some? && idx + 1 < |es| {
            BytesCompareTransitive(k, es[idx + 1].key, hi.value);
          }
        }
      } else {
        assert updated[i] == es[i - m + 1];
      }
    }

    // Prove BranchSorted(updated)
    assert BranchSorted(updated) by {
      forall a, b | 0 <= a < b < |updated|
        ensures BytesLt(updated[a].key, updated[b].key)
      {
        if a < idx && b < idx {
        } else if a < idx && b < idx + m {
          if b == idx {
            assert updated[b].key == es[idx].key;
          } else {
            assert BytesLt(es[a].key, es[idx].key);
            assert InBounds(childRes[b - idx].key, cLo, cHi);
            BytesLtLeTransitive(es[a].key, es[idx].key, childRes[b - idx].key);
          }
        } else if a < idx && b >= idx + m {
          assert updated[b] == es[b - m + 1];
        } else if a >= idx && a < idx + m && b < idx + m {
          if a == idx && idx > 0 {
            TreeNonEmptyKey(newStore, childRes[0].child, childHeight, cLo, ChildHi(childRes, 0, cHi));
            var k0 :| k0 in TreeInterp(newStore, childRes[0].child, childHeight);
            TreeInterpInBounds(newStore, childRes[0].child, childHeight, cLo, ChildHi(childRes, 0, cHi), k0);
            BytesLeLtTransitive(es[idx].key, k0, childRes[1].key);
            if b - idx > 1 {
              BytesCompareTransitive(es[idx].key, childRes[1].key, childRes[b - idx].key);
            }
          }
        } else if a >= idx && a < idx + m && b >= idx + m {
          var origB := b - m + 1;
          assert origB >= idx + 1;
          if a == idx && idx > 0 {
            assert updated[a].key == es[idx].key;
          } else {
            assert InBounds(childRes[a - idx].key, cLo, cHi);
            if origB > idx + 1 {
              BytesCompareTransitive(childRes[a - idx].key, es[idx + 1].key, es[origB].key);
            }
          }
        } else {
          assert updated[a] == es[a - m + 1] && updated[b] == es[b - m + 1];
        }
      }
    }

    // Prove every child in updated is ValidTree with matching bounds
    forall i | 0 <= i < |updated|
      ensures ValidTree(newStore, updated[i].child, childHeight, ChildLo(updated, i, lo), ChildHi(updated, i, hi))
    {
      if i < idx {
        assert updated[i] == es[i];
        assert ChildLo(updated, i, lo) == ChildLo(es, i, lo);
        assert ChildHi(updated, i, hi) == ChildHi(es, i, hi);
      } else if i < idx + m {
        var r := i - idx;
        assert updated[i].child == childRes[r].child;
        assert ChildLo(updated, i, lo) == ChildLo(childRes, r, cLo);
        assert ChildHi(updated, i, hi) == ChildHi(childRes, r, cHi);
      } else {
        var orig := i - m + 1;
        assert updated[i] == es[orig];
        assert ChildLo(updated, i, lo) == ChildLo(es, orig, lo);
        assert ChildHi(updated, i, hi) == ChildHi(es, orig, hi);
      }
    }

    // Prove BranchInterp(newStore, updated) == BranchInterp(oldStore, es)[key := value]
    assert es == es[..idx] + ([es[idx]] + es[idx + 1..]);
    assert updated == es[..idx] + (splicedChild + es[idx + 1..]);
    BranchInterpConcat(oldStore, es[..idx], [es[idx]] + es[idx + 1..], childHeight);
    BranchInterpConcat(oldStore, [es[idx]], es[idx + 1..], childHeight);
    BranchInterpPreserved(oldStore, newStore, es[..idx], childHeight);
    BranchInterpPreserved(oldStore, newStore, es[idx + 1..], childHeight);
    BranchInterpConcat(newStore, es[..idx], splicedChild + es[idx + 1..], childHeight);
    BranchInterpConcat(newStore, splicedChild, es[idx + 1..], childHeight);
    assert BranchInterp(newStore, splicedChild, childHeight) == BranchInterp(newStore, childRes, childHeight);

    var mLeft := BranchInterp(oldStore, es[..idx], childHeight);
    var mMid := TreeInterp(oldStore, es[idx].child, childHeight);
    var mRight := BranchInterp(oldStore, es[idx + 1..], childHeight);
    MapUnionEmptyRight(mMid);
    assert BranchInterp(oldStore, [es[idx]], childHeight) == mMid;
    BranchInterpMember(oldStore, es[..idx], childHeight, key);
    BranchInterpMember(oldStore, es[idx + 1..], childHeight, key);
    assert key !in mLeft && key !in mRight;

    MapUnionPutHelper(mLeft, mMid, mRight, key, value);
  }

  // Allocates one COW BranchPage per chunk and proves ValidPutResults + exact BranchInterp.
  method AllocBranchChunks(
    store: PageStore,
    nextID: PageID,
    chunks: seq<seq<BranchEntry>>,
    ghost childHeight: nat,
    ghost lo: Option<Bytes>,
    ghost hi: Option<Bytes>,
    pageSize: uint32
  ) returns (
    ok: bool,
    newStore: PageStore,
    newNextID: PageID,
    results: seq<BranchEntry>
  )
    requires StoreBoundedBy(store, nextID)
    requires 2 <= nextID
    requires StoreValidPageSizes(store, pageSize)
    requires |chunks| >= 1
    requires forall i | 0 <= i < |chunks| :: |chunks[i]| >= 1
    requires ValidPutResults(store, FlattenBranch(chunks), childHeight, lo, hi)
    requires forall i | 0 <= i < |FlattenBranch(chunks)| :: 24 + 12 + |FlattenBranch(chunks)[i].key| <= pageSize as nat
    ensures ok ==>
      StoreExtends(store, newStore) &&
      StoreBoundedBy(newStore, newNextID) &&
      StoreValidPageSizes(newStore, pageSize) &&
      nextID <= newNextID &&
      |results| == |chunks| &&
      ValidPutResults(newStore, results, childHeight + 1, lo, hi) &&
      (forall i | 0 <= i < |results| :: 24 + 12 + |results[i].key| <= pageSize as nat) &&
      BranchInterp(newStore, results, childHeight + 1) == BranchInterp(store, FlattenBranch(chunks), childHeight)
  {
    var flat := FlattenBranch(chunks);
    BranchChunksOrderLemma(store, chunks, childHeight, lo, hi);

    newStore := store;
    newNextID := nextID;
    results := [];
    var i: nat := 0;
    while i < |chunks|
      invariant i <= |chunks|
      invariant |results| == i
      invariant StoreExtends(store, newStore)
      invariant StoreBoundedBy(newStore, newNextID)
      invariant StoreValidPageSizes(newStore, pageSize)
      invariant nextID <= newNextID
      invariant forall j | 0 <= j < i ::
        results[j].key == chunks[j][0].key &&
        results[j].child in newStore &&
        newStore[results[j].child] == BranchPage(chunks[j]) &&
        24 + 12 + |results[j].key| <= pageSize as nat
    {
      if newNextID == 0xFFFF_FFFF_FFFF_FFFF {
        return false, store, nextID, [];
      }
      var c := chunks[i];
      assert ValidPageEntries(BranchPage(c), pageSize) by {
        forall k | 0 <= k < |c|
          ensures 24 + 12 + |c[k].key| <= pageSize as nat
        {
          FlattenBranchPos(chunks, i, k);
          assert flat[FlattenBranchOffset(chunks, i) + k] == c[k];
        }
      }
      var cid := newNextID;
      newNextID := cid + 1;
      newStore := newStore[cid := BranchPage(c)];
      results := results + [BranchEntry(c[0].key, cid)];
      i := i + 1;
    }

    forall j | 0 <= j < |results|
      ensures ValidTree(newStore, results[j].child, childHeight + 1, ChildLo(results, j, lo), ChildHi(results, j, hi))
    {
      var c := chunks[j];
      var cLoJ := ChildLo(results, j, lo);
      var cHiJ := ChildHi(results, j, hi);
      assert cLoJ == ChunkLo(chunks, j, lo);
      assert cHiJ == ChunkHi(chunks, j, hi);
      assert ValidPutResults(store, chunks[j], childHeight, ChunkLo(chunks, j, lo), ChunkHi(chunks, j, hi));
      forall k | 0 <= k < |c|
        ensures ValidTree(newStore, c[k].child, childHeight, ChildLo(c, k, cLoJ), ChildHi(c, k, cHiJ))
      {
        ValidTreePreserved(store, newStore, c[k].child, childHeight, ChildLo(c, k, cLoJ), ChildHi(c, k, cHiJ));
      }
    }
    forall idx | 0 <= idx < |flat|
      ensures TreeInterp(newStore, flat[idx].child, childHeight) == TreeInterp(store, flat[idx].child, childHeight)
    {
      ValidTreePreserved(store, newStore, flat[idx].child, childHeight, ChildLo(flat, idx, lo), ChildHi(flat, idx, hi));
    }
    BranchChunksInterpLemma(newStore, chunks, results, childHeight);
    BranchInterpPreserved(store, newStore, flat, childHeight);
    ok := true;
  }

  // Recursive COW B+Tree Put on a subtree of height `height`.
  method {:vcs_split_on_every_assert} TreePutNode(
    store: PageStore,
    nextID: PageID,
    pgid: PageID,
    ghost height: nat,
    ghost lo: Option<Bytes>,
    ghost hi: Option<Bytes>,
    key: Bytes,
    value: Bytes,
    pageSize: uint32,
    maxLeafKeys: nat,
    maxBranchKeys: nat
  ) returns (
    ok: bool,
    newStore: PageStore,
    newNextID: PageID,
    results: seq<BranchEntry>
  )
    requires StoreBoundedBy(store, nextID)
    requires 2 <= nextID
    requires StoreValidPageSizes(store, pageSize)
    requires ValidTree(store, pgid, height, lo, hi)
    requires InBounds(key, lo, hi)
    requires 24 + 8 + |key| + |value| <= pageSize as nat
    requires 24 + 2 * (12 + |key|) <= pageSize as nat
    ensures ok ==>
      StoreExtends(store, newStore) &&
      StoreBoundedBy(newStore, newNextID) &&
      StoreValidPageSizes(newStore, pageSize) &&
      nextID <= newNextID &&
      ValidPutResults(newStore, results, height, lo, hi) &&
      (forall i | 0 <= i < |results| :: 24 + 12 + |results[i].key| <= pageSize as nat) &&
      BranchInterp(newStore, results, height) == TreeInterp(store, pgid, height)[key := value]
    decreases height
  {
    match store[pgid]
      case LeafPage(es) =>
        var updated := LeafPut(es, key, value);
        var finalChunks: seq<seq<LeafEntry>>;
        if LeafEncodedSize(updated) <= pageSize as nat && (maxLeafKeys == 0 || |updated| <= maxLeafKeys) {
          finalChunks := [updated];
          assert FlattenLeaf(finalChunks) == updated;
        } else {
          finalChunks := SplitLeafEntries(updated, pageSize, maxLeafKeys);
        }

        newStore := store;
        newNextID := nextID;
        results := [];
        var i: nat := 0;
        while i < |finalChunks|
          invariant i <= |finalChunks|
          invariant |results| == i
          invariant StoreExtends(store, newStore)
          invariant StoreBoundedBy(newStore, newNextID)
          invariant StoreValidPageSizes(newStore, pageSize)
          invariant nextID <= newNextID
          invariant forall j | 0 <= j < i ::
            results[j].key == finalChunks[j][0].key &&
            results[j].child in newStore &&
            newStore[results[j].child] == LeafPage(finalChunks[j]) &&
            24 + 12 + |results[j].key| <= pageSize as nat
        {
          if newNextID == 0xFFFF_FFFF_FFFF_FFFF {
            return false, store, nextID, [];
          }
          var c := finalChunks[i];
          assert ValidPageEntries(LeafPage(c), pageSize) by {
            forall k | 0 <= k < |c|
              ensures 24 + 8 + |c[k].key| + |c[k].value| <= pageSize as nat && 24 + 12 + |c[k].key| <= pageSize as nat
            {
              FlattenLeafIndex(finalChunks, i, k);
              var pos :| 0 <= pos < |updated| && updated[pos] == c[k];
              assert updated[pos] == LeafEntry(key, value) || updated[pos] in es;
            }
          }
          var cid := newNextID;
          newNextID := cid + 1;
          newStore := newStore[cid := LeafPage(c)];
          results := results + [BranchEntry(c[0].key, cid)];
          i := i + 1;
        }
        LeafChunksValidPutResults(newStore, finalChunks, results, lo, hi);
        ok := true;

      case BranchPage(es) =>
        var idx := BranchChildIndex(es, key);
        var cLo := ChildLo(es, idx, lo);
        var cHi := ChildHi(es, idx, hi);
        assert InBounds(key, cLo, cHi);
        BranchRouteKeyUnique(store, es, height - 1, lo, hi, idx, key);

        var childOk, st1, nid1, childRes := TreePutNode(
          store, nextID, es[idx].child, height - 1, cLo, cHi,
          key, value, pageSize, maxLeafKeys, maxBranchKeys
        );
        if !childOk {
          return false, store, nextID, [];
        }

        var splicedFirst :=
          if idx == 0 then childRes[0]
          else BranchEntry(es[idx].key, childRes[0].child);
        var splicedChild := [splicedFirst] + childRes[1..];
        var updated := es[..idx] + splicedChild + es[idx + 1..];

        assert forall k | 0 <= k < |updated| :: 24 + 12 + |updated[k].key| <= pageSize as nat by {
          forall k | 0 <= k < |updated|
            ensures 24 + 12 + |updated[k].key| <= pageSize as nat
          {
            if k < idx {
              assert updated[k] == es[k];
            } else if k < idx + |splicedChild| {
              if k == idx && idx > 0 {
                assert updated[k].key == es[idx].key;
              } else {
                assert updated[k].key == childRes[k - idx].key;
              }
            } else {
              assert updated[k] == es[k - |splicedChild| + 1];
            }
          }
        }

        SpliceBranchValidLemma(store, st1, es, idx, childRes, splicedChild, height - 1, lo, hi, key, value);

        var finalChunks: seq<seq<BranchEntry>>;
        if BranchEncodedSize(updated) <= pageSize as nat && (maxBranchKeys == 0 || |updated| <= maxBranchKeys) {
          finalChunks := [updated];
          assert FlattenBranch(finalChunks) == updated;
        } else {
          finalChunks := SplitBranchEntries(updated, pageSize, maxBranchKeys);
        }

        ok, newStore, newNextID, results := AllocBranchChunks(
          st1, nid1, finalChunks, height - 1, lo, hi, pageSize
        );
  }

  // Top-level COW B+Tree Put (handles empty tree creation and root promotion loop).
  method TreePut(
    store: PageStore,
    nextID: PageID,
    root: PageID,
    ghost height: nat,
    key: Bytes,
    value: Bytes,
    pageSize: uint32,
    maxLeafKeys: nat,
    maxBranchKeys: nat
  ) returns (
    ok: bool,
    newStore: PageStore,
    newNextID: PageID,
    newRoot: PageID,
    ghost newHeight: nat
  )
    requires StoreBoundedBy(store, nextID)
    requires 2 <= nextID
    requires StoreValidPageSizes(store, pageSize)
    requires root != 0 ==> ValidTree(store, root, height, None, None)
    requires |key| > 0
    requires 24 + 8 + |key| + |value| <= pageSize as nat
    requires 24 + 2 * (12 + |key|) <= pageSize as nat
    ensures ok ==>
      StoreExtends(store, newStore) &&
      StoreBoundedBy(newStore, newNextID) &&
      StoreValidPageSizes(newStore, pageSize) &&
      nextID <= newNextID &&
      newRoot != 0 &&
      ValidTree(newStore, newRoot, newHeight, None, None) &&
      TreeInterp(newStore, newRoot, newHeight) ==
        (if root == 0 then map[key := value] else TreeInterp(store, root, height)[key := value])
  {
    if root == 0 {
      if nextID == 0xFFFF_FFFF_FFFF_FFFF {
        return false, store, nextID, 0, 0;
      }
      var cid := nextID;
      var es := [LeafEntry(key, value)];
      assert es[0] == LeafEntry(key, value) && es[1..] == [];
      assert LeafMap(es) == map[key := value];
      newStore := store[cid := LeafPage(es)];
      newNextID := cid + 1;
      newRoot := cid;
      newHeight := 0;
      ok := true;
      return;
    }

    var putOk, st, nid, res := TreePutNode(
      store, nextID, root, height, None, None,
      key, value, pageSize, maxLeafKeys, maxBranchKeys
    );
    if !putOk {
      return false, store, nextID, 0, 0;
    }

    var curHeight: nat := height;
    var curStore := st;
    var curNextID := nid;
    var curRes := res;

    // Root promotion loop (matches engine/btree.go lines 260-279)
    while |curRes| > 1
      invariant StoreExtends(store, curStore)
      invariant StoreBoundedBy(curStore, curNextID)
      invariant StoreValidPageSizes(curStore, pageSize)
      invariant nextID <= curNextID
      invariant ValidPutResults(curStore, curRes, curHeight, None, None)
      invariant forall i | 0 <= i < |curRes| :: 24 + 12 + |curRes[i].key| <= pageSize as nat
      invariant BranchInterp(curStore, curRes, curHeight) == TreeInterp(store, root, height)[key := value]
      decreases |curRes|
    {
      if BranchEncodedSize(curRes) <= pageSize as nat && (maxBranchKeys == 0 || |curRes| <= maxBranchKeys) {
        if curNextID == 0xFFFF_FFFF_FFFF_FFFF {
          return false, store, nextID, 0, 0;
        }
        var rid := curNextID;
        var stNew := curStore[rid := BranchPage(curRes)];
        forall i | 0 <= i < |curRes|
          ensures ValidTree(stNew, curRes[i].child, curHeight, ChildLo(curRes, i, None), ChildHi(curRes, i, None))
          ensures TreeInterp(stNew, curRes[i].child, curHeight) == TreeInterp(curStore, curRes[i].child, curHeight)
        {
          ValidTreePreserved(curStore, stNew, curRes[i].child, curHeight, ChildLo(curRes, i, None), ChildHi(curRes, i, None));
        }
        BranchInterpPreserved(curStore, stNew, curRes, curHeight);
        return true, stNew, rid + 1, rid, curHeight + 1;
      }

      var chunks := SplitBranchEntries(curRes, pageSize, maxBranchKeys);
      if |chunks| >= |curRes| {
        // Impossible when each entry fits 2 per page, guarded for termination proof
        return false, store, nextID, 0, 0;
      }
      BranchChunksOrderLemma(curStore, chunks, curHeight, None, None);

      var stNext := curStore;
      var nidNext := curNextID;
      var nextLevel: seq<BranchEntry> := [];
      var i: nat := 0;
      while i < |chunks|
        invariant i <= |chunks|
        invariant |nextLevel| == i
        invariant StoreExtends(curStore, stNext)
        invariant StoreBoundedBy(stNext, nidNext)
        invariant StoreValidPageSizes(stNext, pageSize)
        invariant curNextID <= nidNext
        invariant forall j | 0 <= j < i ::
          nextLevel[j].key == chunks[j][0].key &&
          nextLevel[j].child in stNext &&
          stNext[nextLevel[j].child] == BranchPage(chunks[j]) &&
          24 + 12 + |nextLevel[j].key| <= pageSize as nat
      {
        if nidNext == 0xFFFF_FFFF_FFFF_FFFF {
          return false, store, nextID, 0, 0;
        }
        var c := chunks[i];
        assert ValidPageEntries(BranchPage(c), pageSize) by {
          forall k | 0 <= k < |c|
            ensures 24 + 12 + |c[k].key| <= pageSize as nat
          {
            FlattenBranchPos(chunks, i, k);
            assert curRes[FlattenBranchOffset(chunks, i) + k] == c[k];
          }
        }
        var cid := nidNext;
        nidNext := cid + 1;
        stNext := stNext[cid := BranchPage(c)];
        nextLevel := nextLevel + [BranchEntry(c[0].key, cid)];
        i := i + 1;
      }

      forall j | 0 <= j < |nextLevel|
        ensures ValidTree(stNext, nextLevel[j].child, curHeight + 1, ChildLo(nextLevel, j, None), ChildHi(nextLevel, j, None))
      {
        var c := chunks[j];
        var cLoJ := ChildLo(nextLevel, j, None);
        var cHiJ := ChildHi(nextLevel, j, None);
        assert cLoJ == ChunkLo(chunks, j, None);
        assert cHiJ == ChunkHi(chunks, j, None);
        assert ValidPutResults(curStore, chunks[j], curHeight, ChunkLo(chunks, j, None), ChunkHi(chunks, j, None));
        forall k | 0 <= k < |c|
          ensures ValidTree(stNext, c[k].child, curHeight, ChildLo(c, k, cLoJ), ChildHi(c, k, cHiJ))
        {
          ValidTreePreserved(curStore, stNext, c[k].child, curHeight, ChildLo(c, k, cLoJ), ChildHi(c, k, cHiJ));
        }
      }
      forall i | 0 <= i < |curRes|
        ensures TreeInterp(stNext, curRes[i].child, curHeight) == TreeInterp(curStore, curRes[i].child, curHeight)
      {
        ValidTreePreserved(curStore, stNext, curRes[i].child, curHeight, ChildLo(curRes, i, None), ChildHi(curRes, i, None));
      }
      BranchChunksInterpLemma(stNext, chunks, nextLevel, curHeight);
      BranchInterpPreserved(curStore, stNext, curRes, curHeight);

      curStore := stNext;
      curNextID := nidNext;
      curRes := nextLevel;
      curHeight := curHeight + 1;
    }

    assert |curRes| == 1;
    assert BranchInterp(curStore, curRes, curHeight) == TreeInterp(curStore, curRes[0].child, curHeight);
    return true, curStore, curNextID, curRes[0].child, curHeight;
  }

  // =========================================================================
  // Verified TreeDelete — COW Deletion + Empty Node Pruning + Root Collapse
  // =========================================================================

  lemma PreserveAllBranchChildren(
    oldStore: PageStore,
    newStore: PageStore,
    es: seq<BranchEntry>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>
  )
    requires StoreExtends(oldStore, newStore)
    requires forall i | 0 <= i < |es| ::
      ValidTree(oldStore, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi))
    ensures forall i | 0 <= i < |es| ::
      ValidTree(newStore, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi)) &&
      TreeInterp(newStore, es[i].child, childHeight) == TreeInterp(oldStore, es[i].child, childHeight)
    ensures BranchInterp(newStore, es, childHeight) == BranchInterp(oldStore, es, childHeight)
  {
    var i: nat := 0;
    while i < |es|
      invariant i <= |es|
      invariant forall j | 0 <= j < i ::
        ValidTree(newStore, es[j].child, childHeight, ChildLo(es, j, lo), ChildHi(es, j, hi)) &&
        TreeInterp(newStore, es[j].child, childHeight) == TreeInterp(oldStore, es[j].child, childHeight)
    {
      ValidTreePreserved(oldStore, newStore, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi));
      i := i + 1;
    }
    BranchInterpPreserved(oldStore, newStore, es, childHeight);
  }

  method {:vcs_split_on_every_assert} TreeDeleteNode(
    store: PageStore,
    nextID: PageID,
    pgid: PageID,
    ghost height: nat,
    ghost lo: Option<Bytes>,
    ghost hi: Option<Bytes>,
    key: Bytes,
    isRoot: bool,
    pageSize: uint32
  ) returns (
    ok: bool,
    newStore: PageStore,
    newNextID: PageID,
    newID: PageID,
    ghost newHeight: nat,
    deleted: bool
  )
    requires StoreBoundedBy(store, nextID)
    requires 2 <= nextID
    requires StoreValidPageSizes(store, pageSize)
    requires ValidTree(store, pgid, height, lo, hi)
    requires isRoot ==> lo.None? && hi.None?
    ensures ok ==>
      StoreExtends(store, newStore) &&
      StoreBoundedBy(newStore, newNextID) &&
      StoreValidPageSizes(newStore, pageSize) &&
      nextID <= newNextID &&
      (deleted <==> key in TreeInterp(store, pgid, height)) &&
      (!deleted ==> newID == pgid && newHeight == height && newStore == store) &&
      (newID == 0 ==> TreeInterp(store, pgid, height) - {key} == map[]) &&
      (newID != 0 ==>
        (!isRoot ==> newHeight == height) &&
        ValidTree(newStore, newID, newHeight, lo, hi) &&
        TreeInterp(newStore, newID, newHeight) == TreeInterp(store, pgid, height) - {key})
    decreases height
  {
    match store[pgid]
      case LeafPage(es) =>
        var updated, del := LeafDelete(es, key);
        if !del {
          return true, store, nextID, pgid, height, false;
        }
        if |updated| == 0 {
          return true, store, nextID, 0, 0, true;
        }
        if nextID == 0xFFFF_FFFF_FFFF_FFFF {
          return false, store, nextID, pgid, height, false;
        }
        var cid := nextID;
        newStore := store[cid := LeafPage(updated)];
        return true, newStore, cid + 1, cid, 0, true;

      case BranchPage(es) =>
        var idx := BranchChildIndex(es, key);
        var cLo := ChildLo(es, idx, lo);
        var cHi := ChildHi(es, idx, hi);
        BranchRouteKeyUnique(store, es, height - 1, lo, hi, idx, key);

        var childOk, st1, nid1, childNewID, _, childDel := TreeDeleteNode(
          store, nextID, es[idx].child, height - 1, cLo, cHi, key, false, pageSize
        );
        if !childOk {
          return false, store, nextID, pgid, height, false;
        }
        if !childDel {
          return true, store, nextID, pgid, height, false;
        }

        var updated: seq<BranchEntry>;
        if childNewID == 0 {
          updated := es[..idx] + es[idx + 1..];
        } else {
          updated := es[..idx] + [BranchEntry(es[idx].key, childNewID)] + es[idx + 1..];
        }

        PreserveAllBranchChildren(store, st1, es, height - 1, lo, hi);
        DeleteBranchInterpLemma(store, st1, es, idx, childNewID, updated, height - 1, key);

        if |updated| == 0 {
          return true, st1, nid1, 0, 0, true;
        }

        DeleteBranchValidLemma(store, st1, es, idx, childNewID, updated, height - 1, lo, hi);

        if isRoot && |updated| == 1 {
          MapUnionEmptyRight(TreeInterp(st1, updated[0].child, height - 1));
          assert ValidTree(st1, updated[0].child, height - 1, None, None);
          assert TreeInterp(st1, updated[0].child, height - 1) == BranchInterp(st1, updated, height - 1);
          return true, st1, nid1, updated[0].child, height - 1, true;
        }

        if nid1 == 0xFFFF_FFFF_FFFF_FFFF {
          return false, store, nextID, pgid, height, false;
        }
        var cid := nid1;
        var st2 := st1[cid := BranchPage(updated)];
        PreserveAllBranchChildren(st1, st2, updated, height - 1, lo, hi);
        return true, st2, cid + 1, cid, height, true;
  }

  // Top-level COW B+Tree Delete (handles empty tree cleanly).
  method TreeDelete(
    store: PageStore,
    nextID: PageID,
    root: PageID,
    ghost height: nat,
    key: Bytes,
    pageSize: uint32
  ) returns (
    ok: bool,
    newStore: PageStore,
    newNextID: PageID,
    newRoot: PageID,
    ghost newHeight: nat,
    deleted: bool
  )
    requires StoreBoundedBy(store, nextID)
    requires 2 <= nextID
    requires StoreValidPageSizes(store, pageSize)
    requires root != 0 ==> ValidTree(store, root, height, None, None)
    ensures ok ==>
      StoreExtends(store, newStore) &&
      StoreBoundedBy(newStore, newNextID) &&
      StoreValidPageSizes(newStore, pageSize) &&
      nextID <= newNextID &&
      (root == 0 ==> newRoot == 0 && !deleted && newStore == store) &&
      (root != 0 ==>
        (deleted <==> key in TreeInterp(store, root, height)) &&
        (newRoot == 0 ==> TreeInterp(store, root, height) - {key} == map[]) &&
        (newRoot != 0 ==>
          ValidTree(newStore, newRoot, newHeight, None, None) &&
          TreeInterp(newStore, newRoot, newHeight) == TreeInterp(store, root, height) - {key}))
  {
    if root == 0 {
      return true, store, nextID, 0, 0, false;
    }
    ok, newStore, newNextID, newRoot, newHeight, deleted :=
      TreeDeleteNode(store, nextID, root, height, None, None, key, true, pageSize);
  }

  lemma MapUnionDeleteHelper(mLeft: map<Bytes, Bytes>, mMid: map<Bytes, Bytes>, mRight: map<Bytes, Bytes>, key: Bytes)
    requires key !in mLeft && key !in mRight
    ensures MapUnion(mLeft, MapUnion(mMid - {key}, mRight)) == MapUnion(mLeft, MapUnion(mMid, mRight)) - {key}
    ensures mMid - {key} == map[] ==> MapUnion(mLeft, mRight) == MapUnion(mLeft, MapUnion(mMid, mRight)) - {key}
  {
    var full := MapUnion(mLeft, MapUnion(mMid, mRight));
    var expected := full - {key};
    var u1 := MapUnion(mLeft, MapUnion(mMid - {key}, mRight));
    forall k | k in u1 ensures k in expected && u1[k] == expected[k] {}
    forall k | k in expected ensures k in u1 && u1[k] == expected[k] {}
    assert u1 == expected;
    if mMid - {key} == map[] {
      var u2 := MapUnion(mLeft, mRight);
      forall k | k in u2 ensures k in expected && u2[k] == expected[k] {}
      forall k | k in expected ensures k in u2 && u2[k] == expected[k] {}
      assert u2 == expected;
    }
  }

  lemma DeleteBranchInterpLemma(
    oldStore: PageStore,
    newStore: PageStore,
    es: seq<BranchEntry>,
    idx: nat,
    childNewID: PageID,
    updated: seq<BranchEntry>,
    childHeight: nat,
    key: Bytes
  )
    requires idx < |es|
    requires forall i | 0 <= i < |es| ::
      TreeInterp(newStore, es[i].child, childHeight) == TreeInterp(oldStore, es[i].child, childHeight)
    requires forall j | 0 <= j < |es| && j != idx :: key !in TreeInterp(oldStore, es[j].child, childHeight)
    requires childNewID == 0 ==> TreeInterp(oldStore, es[idx].child, childHeight) - {key} == map[] && updated == es[..idx] + es[idx + 1..]
    requires childNewID != 0 ==>
      TreeInterp(newStore, childNewID, childHeight) == TreeInterp(oldStore, es[idx].child, childHeight) - {key} &&
      updated == es[..idx] + [BranchEntry(es[idx].key, childNewID)] + es[idx + 1..]
    ensures BranchInterp(newStore, updated, childHeight) == BranchInterp(oldStore, es, childHeight) - {key}
  {
    assert es == es[..idx] + ([es[idx]] + es[idx + 1..]);
    BranchInterpConcat(oldStore, es[..idx], [es[idx]] + es[idx + 1..], childHeight);
    BranchInterpConcat(oldStore, [es[idx]], es[idx + 1..], childHeight);
    BranchInterpPreserved(oldStore, newStore, es[..idx], childHeight);
    BranchInterpPreserved(oldStore, newStore, es[idx + 1..], childHeight);
    BranchInterpMember(oldStore, es[..idx], childHeight, key);
    BranchInterpMember(oldStore, es[idx + 1..], childHeight, key);

    var mLeft := BranchInterp(oldStore, es[..idx], childHeight);
    var mMid := TreeInterp(oldStore, es[idx].child, childHeight);
    var mRight := BranchInterp(oldStore, es[idx + 1..], childHeight);
    MapUnionEmptyRight(mMid);
    assert BranchInterp(oldStore, [es[idx]], childHeight) == mMid;
    assert BranchInterp(oldStore, es, childHeight) == MapUnion(mLeft, MapUnion(mMid, mRight));
    assert key !in mLeft && key !in mRight;

    MapUnionDeleteHelper(mLeft, mMid, mRight, key);
    if childNewID == 0 {
      BranchInterpConcat(newStore, es[..idx], es[idx + 1..], childHeight);
      assert BranchInterp(newStore, updated, childHeight) == MapUnion(mLeft, mRight);
    } else {
      var repl := [BranchEntry(es[idx].key, childNewID)];
      assert updated == es[..idx] + (repl + es[idx + 1..]);
      var mMidNew := TreeInterp(newStore, childNewID, childHeight);
      MapUnionEmptyRight(mMidNew);
      assert BranchInterp(newStore, repl, childHeight) == mMidNew;
      BranchInterpConcat(newStore, es[..idx], repl + es[idx + 1..], childHeight);
      BranchInterpConcat(newStore, repl, es[idx + 1..], childHeight);
      assert BranchInterp(newStore, updated, childHeight) == MapUnion(mLeft, MapUnion(mMidNew, mRight));
    }
  }

  lemma DeleteBranchValidLemma(
    oldStore: PageStore,
    newStore: PageStore,
    es: seq<BranchEntry>,
    idx: nat,
    childNewID: PageID,
    updated: seq<BranchEntry>,
    childHeight: nat,
    lo: Option<Bytes>,
    hi: Option<Bytes>
  )
    requires idx < |es|
    requires BranchSorted(es)
    requires forall i | 0 <= i < |es| :: InBounds(es[i].key, lo, hi)
    requires forall i | 0 <= i < |es| ::
      ValidTree(newStore, es[i].child, childHeight, ChildLo(es, i, lo), ChildHi(es, i, hi))
    requires childNewID == 0 ==> updated == es[..idx] + es[idx + 1..]
    requires childNewID != 0 ==>
      ValidTree(newStore, childNewID, childHeight, ChildLo(es, idx, lo), ChildHi(es, idx, hi)) &&
      updated == es[..idx] + [BranchEntry(es[idx].key, childNewID)] + es[idx + 1..]
    ensures BranchSorted(updated)
    ensures forall i | 0 <= i < |updated| :: InBounds(updated[i].key, lo, hi)
    ensures forall i | 0 <= i < |updated| ::
      ValidTree(newStore, updated[i].child, childHeight, ChildLo(updated, i, lo), ChildHi(updated, i, hi))
  {
    if childNewID != 0 {
      forall i | 0 <= i < |updated|
        ensures ValidTree(newStore, updated[i].child, childHeight, ChildLo(updated, i, lo), ChildHi(updated, i, hi))
      {
        assert ChildLo(updated, i, lo) == ChildLo(es, i, lo);
        assert ChildHi(updated, i, hi) == ChildHi(es, i, hi);
      }
    } else {
      forall i | 0 <= i < |updated|
        ensures ValidTree(newStore, updated[i].child, childHeight, ChildLo(updated, i, lo), ChildHi(updated, i, hi))
      {
        var orig := if i < idx then i else i + 1;
        assert updated[i] == es[orig];
        var oLo := ChildLo(es, orig, lo);
        var oHi := ChildHi(es, orig, hi);
        var nLo := ChildLo(updated, i, lo);
        var nHi := ChildHi(updated, i, hi);
        if nLo.Some? && oLo.Some? {
          if nLo.value == oLo.value {
            BytesCompareReflexive(nLo.value);
          }
        }
        if nHi.Some? && oHi.Some? {
          if nHi.value == oHi.value {
            BytesCompareReflexive(nHi.value);
          } else {
            assert BytesLt(oHi.value, nHi.value);
          }
        }
        ValidTreeWiden(newStore, es[orig].child, childHeight, oLo, oHi, nLo, nHi);
      }
    }
  }

  // =========================================================================
  // Verified Executable Structural Validator (Matches engine/btree.go btreeValidate)
  // =========================================================================

  method ValidateLeafEntries(entries: seq<LeafEntry>) returns (ok: bool)
    ensures ok <==> |entries| >= 1 && LeafSorted(entries)
  {
    if |entries| == 0 {
      return false;
    }
    var i: nat := 1;
    while i < |entries|
      invariant 1 <= i <= |entries|
      invariant LeafSorted(entries[..i])
    {
      if BytesCompare(entries[i - 1].key, entries[i].key) >= 0 {
        return false;
      }
      assert LeafSorted(entries[..i + 1]) by {
        forall a, b | 0 <= a < b < i + 1
          ensures BytesLt(entries[a].key, entries[b].key)
        {
          if b == i && a < i - 1 {
            BytesCompareTransitive(entries[a].key, entries[i - 1].key, entries[i].key);
          }
        }
      }
      i := i + 1;
    }
    assert entries[..|entries|] == entries;
    return true;
  }

  method TreeValidateNode(
    store: PageStore,
    pgid: PageID,
    highWater: PageID,
    visited: set<PageID>,
    fuel: nat
  ) returns (
    ok: bool,
    minKey: Bytes,
    maxKey: Bytes,
    newVisited: set<PageID>
  )
    ensures visited <= newVisited
    decreases fuel
  {
    if fuel == 0 || pgid < 2 || pgid >= highWater || pgid in visited || pgid !in store {
      return false, [], [], visited;
    }
    newVisited := visited + {pgid};
    match store[pgid]
      case LeafPage(es) =>
        var leafOk := ValidateLeafEntries(es);
        if !leafOk {
          return false, [], [], newVisited;
        }
        return true, es[0].key, es[|es| - 1].key, newVisited;

      case BranchPage(es) =>
        if |es| == 0 {
          return false, [], [], newVisited;
        }
        var firstMin: Bytes := [];
        var prevMax: Bytes := [];
        var i: nat := 0;
        while i < |es|
          invariant i <= |es|
          invariant visited <= newVisited
        {
          var childOk, cMin, cMax, nextVis := TreeValidateNode(store, es[i].child, highWater, newVisited, fuel - 1);
          newVisited := nextVis;
          if !childOk {
            return false, [], [], newVisited;
          }
          if i == 0 {
            firstMin := cMin;
          } else {
            if BytesCompare(prevMax, cMin) >= 0 {
              return false, [], [], newVisited;
            }
            if BytesCompare(es[i].key, cMin) > 0 {
              return false, [], [], newVisited;
            }
          }
          prevMax := cMax;
          i := i + 1;
        }
        return true, firstMin, prevMax, newVisited;
  }

  method TreeValidate(store: PageStore, root: PageID, highWater: PageID) returns (ok: bool)
  {
    if root == 0 {
      return true;
    }
    var valid, _, _, _ := TreeValidateNode(store, root, highWater, {}, MaxBTreeDepth);
    return valid;
  }
}
