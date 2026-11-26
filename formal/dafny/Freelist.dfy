// GroundKV Phase 1 — Verified MVCC Freelist Sizing, Release Bound, and Pool Operations
// Matches engine/freelist.go, engine/db.go (releaseBoundLocked), and engine/tx.go (prepareFreelistPages).

include "Types.dfy"

module GroundKVFreelist {
  import opened GroundKVTypes

  // =========================================================================
  // MVCC + Dual-Meta Release Bound
  // =========================================================================

  function ReleaseBound(currentTxID: uint64, hasReader: bool, minActiveReaderTxID: uint64): uint64
    ensures currentTxID == 0 ==> ReleaseBound(currentTxID, hasReader, minActiveReaderTxID) == 0
    ensures currentTxID > 0 ==> ReleaseBound(currentTxID, hasReader, minActiveReaderTxID) <= currentTxID - 1
    ensures hasReader ==> ReleaseBound(currentTxID, hasReader, minActiveReaderTxID) <= minActiveReaderTxID
  {
    var dualMetaBound: uint64 := if currentTxID > 0 then currentTxID - 1 else 0;
    if hasReader && minActiveReaderTxID < dualMetaBound then
      minActiveReaderTxID
    else
      dualMetaBound
  }

  // Theorem: any page freed at freeTxID <= ReleaseBound is strictly older than
  // both on-disk meta slots (currentTxID and currentTxID-1) and every active
  // MVCC reader snapshot (readerTxID >= minActiveReaderTxID).
  lemma ReleaseBoundProtectsAllLiveSnapshots(
    currentTxID: uint64,
    hasReader: bool,
    minActiveReaderTxID: uint64,
    freeTxID: uint64,
    liveSnapTxID: uint64
  )
    requires freeTxID <= ReleaseBound(currentTxID, hasReader, minActiveReaderTxID)
    requires liveSnapTxID == currentTxID ||
             (currentTxID > 0 && liveSnapTxID == currentTxID - 1) ||
             (hasReader && liveSnapTxID >= minActiveReaderTxID)
    ensures freeTxID <= liveSnapTxID
  {}

  // =========================================================================
  // Self-Referential Freelist Chain Sizing Formula (Sufficiency + Minimality)
  // =========================================================================

  function RemainingAfterAlloc(nonReusable: nat, reusable: nat, c: nat): nat
  {
    nonReusable + (if reusable > c then reusable - c else 0)
  }

  function ComputeFreelistPageCount(initialAllLen: nat, freeCount: nat, localFreeLen: nat, perPage: nat): nat
    requires perPage >= 1
    requires freeCount <= initialAllLen
  {
    var total := initialAllLen + localFreeLen;
    var nonReusable := initialAllLen - freeCount;
    var c1 := (total + perPage) / (perPage + 1);
    var c2 := if nonReusable > 0 then (nonReusable + perPage - 1) / perPage else 0;
    var c := if c2 > c1 then c2 else c1;
    if c < 1 then 1 else c
  }

  lemma CeilDivGe(a: nat, d: nat)
    requires d >= 1
    ensures ((a + d - 1) / d) * d >= a
  {
    var q := (a + d - 1) / d;
    var r := (a + d - 1) % d;
    assert q * d + r == a + d - 1;
    assert 0 <= r < d;
  }

  lemma MulLeMonotone(a: nat, b: nat, k: nat)
    requires a <= b
    ensures a * k <= b * k
  {}

  // Theorem 1 (Sufficiency): allocating C pages from (reusable = freeCount + localFreeLen)
  // leaves RemainingAfterAlloc(nonReusable, reusable, C) <= C * perPage, so the
  // persisted freelist chain ALWAYS fits with zero overflow.
  lemma FreelistPageCountSufficient(initialAllLen: nat, freeCount: nat, localFreeLen: nat, perPage: nat)
    requires perPage >= 1
    requires freeCount <= initialAllLen
    ensures var c := ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage);
            var nonReusable := initialAllLen - freeCount;
            var reusable := freeCount + localFreeLen;
            c >= 1 && RemainingAfterAlloc(nonReusable, reusable, c) <= c * perPage
  {
    var total := initialAllLen + localFreeLen;
    var nonReusable := initialAllLen - freeCount;
    var reusable := freeCount + localFreeLen;
    assert total == nonReusable + reusable;

    var c1 := (total + perPage) / (perPage + 1);
    var c2 := if nonReusable > 0 then (nonReusable + perPage - 1) / perPage else 0;
    var c := ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage);

    assert c >= c1;
    assert c >= c2;
    assert c >= 1;

    // Case 1: c >= reusable ==> remaining == nonReusable <= c2 * perPage <= c * perPage
    if c >= reusable {
      if nonReusable > 0 {
        CeilDivGe(nonReusable, perPage);
        MulLeMonotone(c2, c, perPage);
        assert c * perPage >= nonReusable;
      }
    } else {
      // Case 2: c < reusable ==> remaining == total - c <= c * perPage <==> total <= c * (perPage + 1)
      CeilDivGe(total, perPage + 1);
      MulLeMonotone(c1, c, perPage + 1);
      assert c * (perPage + 1) >= total;
      assert c * (perPage + 1) == c * perPage + c;
    }
  }

  // Theorem 2 (Strict Minimality): for every total >= 1, C-1 pages would strictly
  // overflow (when C > 1), so ComputeFreelistPageCount is exact and optimal.
  lemma FreelistPageCountMinimal(initialAllLen: nat, freeCount: nat, localFreeLen: nat, perPage: nat)
    requires perPage >= 1
    requires freeCount <= initialAllLen
    ensures var c := ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage);
            var nonReusable := initialAllLen - freeCount;
            var reusable := freeCount + localFreeLen;
            c > 1 ==> RemainingAfterAlloc(nonReusable, reusable, c - 1) > (c - 1) * perPage
  {
    var total := initialAllLen + localFreeLen;
    var nonReusable := initialAllLen - freeCount;
    var reusable := freeCount + localFreeLen;
    assert total == nonReusable + reusable;

    var c1 := (total + perPage) / (perPage + 1);
    var c2 := if nonReusable > 0 then (nonReusable + perPage - 1) / perPage else 0;
    var c := ComputeFreelistPageCount(initialAllLen, freeCount, localFreeLen, perPage);

    if c > 1 {
      if c == c2 && nonReusable > 0 {
        assert (c2 - 1) * perPage < nonReusable;
        assert RemainingAfterAlloc(nonReusable, reusable, c - 1) >= nonReusable;
      } else {
        assert c == c1;
        assert (c1 - 1) * (perPage + 1) < total;
        assert (c - 1) * (perPage + 1) == (c - 1) * perPage + (c - 1);
        if reusable > c - 1 {
          assert RemainingAfterAlloc(nonReusable, reusable, c - 1) == total - (c - 1);
        } else {
          // If reusable <= c - 1, then nonReusable == total - reusable >= total - (c - 1) > (c - 1) * perPage
          assert RemainingAfterAlloc(nonReusable, reusable, c - 1) == nonReusable;
          assert nonReusable >= total - (c - 1);
        }
      }
    }
  }

  // =========================================================================
  // Verified Freelist Pool State & Operations
  // =========================================================================

  datatype FreelistState = FreelistState(
    free: set<PageID>,
    pending: map<uint64, set<PageID>>
  )

  predicate ValidFreelist(fl: FreelistState)
  {
    (forall id | id in fl.free :: id >= 2) &&
    (forall txid | txid in fl.pending :: forall id | id in fl.pending[txid] :: id >= 2)
  }

  function EmptyFreelist(): FreelistState
    ensures ValidFreelist(EmptyFreelist())
  {
    FreelistState({}, map[])
  }

  function FilterValidIDs(ids: set<PageID>): set<PageID>
    ensures forall id | id in FilterValidIDs(ids) :: id >= 2
  {
    set id | id in ids && id >= 2
  }

  function AddPending(fl: FreelistState, txid: uint64, ids: set<PageID>): FreelistState
    requires ValidFreelist(fl)
    ensures ValidFreelist(AddPending(fl, txid, ids))
    ensures AddPending(fl, txid, ids).free == fl.free
  {
    var clean := FilterValidIDs(ids);
    if clean == {} then fl
    else
      var existing := if txid in fl.pending then fl.pending[txid] else {};
      fl.(pending := fl.pending[txid := existing + clean])
  }

  function ReturnToFree(fl: FreelistState, ids: set<PageID>): FreelistState
    requires ValidFreelist(fl)
    ensures ValidFreelist(ReturnToFree(fl, ids))
    ensures ReturnToFree(fl, ids).pending == fl.pending
  {
    fl.(free := fl.free + FilterValidIDs(ids))
  }

  function PendingUnionUpTo(pending: map<uint64, set<PageID>>, maxSafeTxID: uint64): set<PageID>
  {
    set txid, id | txid in pending && txid <= maxSafeTxID && id in pending[txid] :: id
  }

  function ReleaseEligible(fl: FreelistState, maxSafeTxID: uint64): FreelistState
    requires ValidFreelist(fl)
    ensures ValidFreelist(ReleaseEligible(fl, maxSafeTxID))
    ensures forall txid | txid in ReleaseEligible(fl, maxSafeTxID).pending :: txid > maxSafeTxID
    ensures forall id | id in fl.free :: id in ReleaseEligible(fl, maxSafeTxID).free
    ensures forall txid, id | txid in fl.pending && txid <= maxSafeTxID && id in fl.pending[txid] ::
      id in ReleaseEligible(fl, maxSafeTxID).free
    ensures forall txid | txid in fl.pending && txid > maxSafeTxID ::
      txid in ReleaseEligible(fl, maxSafeTxID).pending &&
      ReleaseEligible(fl, maxSafeTxID).pending[txid] == fl.pending[txid]
  {
    var released := PendingUnionUpTo(fl.pending, maxSafeTxID);
    var remaining := map txid | txid in fl.pending && txid > maxSafeTxID :: fl.pending[txid];
    FreelistState(fl.free + released, remaining)
  }

  // Deterministic lowest-PageID extraction (matches engine/freelist.go pop()).
  method PopMinFree(fl: FreelistState) returns (nextFl: FreelistState, pgid: PageID, ok: bool)
    requires ValidFreelist(fl)
    ensures ValidFreelist(nextFl)
    ensures nextFl.pending == fl.pending
    ensures !ok ==> fl.free == {} && nextFl == fl && pgid == 0
    ensures ok ==>
      pgid in fl.free &&
      pgid >= 2 &&
      nextFl.free == fl.free - {pgid} &&
      (forall x | x in fl.free :: pgid <= x)
  {
    if fl.free == {} {
      return fl, 0, false;
    }
    var remaining := fl.free;
    var best :| best in remaining;
    var seen: set<PageID> := {best};
    remaining := remaining - {best};
    while remaining != {}
      invariant best in seen
      invariant seen + remaining == fl.free
      invariant seen * remaining == {}
      invariant forall x | x in seen :: best <= x
      decreases |remaining|
    {
      var x :| x in remaining;
      if x < best {
        best := x;
      }
      seen := seen + {x};
      remaining := remaining - {x};
    }
    return fl.(free := fl.free - {best}), best, true;
  }

  // Strictly sorted snapshot of all free, pending, and extra IDs >= 2.
  predicate StrictlySortedIDs(s: seq<PageID>)
  {
    forall i, j | 0 <= i < j < |s| :: s[i] < s[j]
  }

  // O(N) minimum PageID scan over a non-empty sequence (avoids quadratic Set.Difference in Go).
  method MinPageIDInSeq(s: seq<PageID>) returns (best: PageID)
    requires |s| >= 1
    ensures best in s
    ensures forall i | 0 <= i < |s| :: best <= s[i]
  {
    best := s[0];
    var i: nat := 1;
    while i < |s|
      invariant 1 <= i <= |s|
      invariant best in s[..i]
      invariant forall j | 0 <= j < i :: best <= s[j]
    {
      if s[i] < best {
        best := s[i];
      }
      i := i + 1;
    }
  }

  // Binary search insertion into a strictly sorted PageID sequence, deduplicating if present.
  method InsertPageIDSortedDedup(s: seq<PageID>, x: PageID) returns (out: seq<PageID>)
    requires StrictlySortedIDs(s)
    ensures StrictlySortedIDs(out)
    ensures forall y :: y in out <==> (y in s || y == x)
    ensures x in s ==> out == s
    ensures x !in s ==> |out| == |s| + 1
  {
    var lo: nat := 0;
    var hi: nat := |s|;
    while lo < hi
      invariant lo <= hi <= |s|
      invariant forall i | 0 <= i < lo :: s[i] < x
      invariant forall i | hi <= i < |s| :: x <= s[i]
    {
      var mid := lo + (hi - lo) / 2;
      if s[mid] < x {
        lo := mid + 1;
      } else {
        hi := mid;
      }
    }
    if lo < |s| && s[lo] == x {
      return s;
    }
    out := s[..lo] + [x] + s[lo..];
    assert forall y :: y in out <==> (y in s || y == x) by {
      forall y | y in out ensures y in s || y == x {
        var i :| 0 <= i < |out| && out[i] == y;
        if i < lo {
          assert s[i] == y;
        } else if i == lo {
          assert y == x;
        } else {
          assert s[i - 1] == y;
        }
      }
      forall y | y in s || y == x ensures y in out {
        if y == x {
          assert out[lo] == x;
        } else {
          var i :| 0 <= i < |s| && s[i] == y;
          if i < lo {
            assert out[i] == y;
          } else {
            assert out[i + 1] == y;
          }
        }
      }
    }
  }

  // Verified strictly-sorted deduplication of a PageID sequence (zero Set.Difference overhead).
  method SortDedupPageIDs(s: seq<PageID>) returns (out: seq<PageID>)
    ensures StrictlySortedIDs(out)
    ensures forall x :: x in out <==> x in s
  {
    out := [];
    var i: nat := 0;
    while i < |s|
      invariant i <= |s|
      invariant StrictlySortedIDs(out)
      invariant forall x :: x in out <==> x in s[..i]
    {
      out := InsertPageIDSortedDedup(out, s[i]);
      assert s[..i + 1] == s[..i] + [s[i]];
      i := i + 1;
    }
    assert s[..|s|] == s;
  }

  // Binary search insertion into a strictly sorted PageID sequence (O(log N) comparisons).
  method InsertPageIDSorted(s: seq<PageID>, x: PageID) returns (out: seq<PageID>)
    requires StrictlySortedIDs(s)
    requires x !in s
    ensures StrictlySortedIDs(out)
    ensures |out| == |s| + 1
    ensures forall y :: y in out <==> (y in s || y == x)
  {
    var lo: nat := 0;
    var hi: nat := |s|;
    while lo < hi
      invariant lo <= hi <= |s|
      invariant forall i | 0 <= i < lo :: s[i] < x
      invariant forall i | hi <= i < |s| :: x < s[i]
    {
      var mid := lo + (hi - lo) / 2;
      if s[mid] < x {
        lo := mid + 1;
      } else {
        assert s[mid] != x;
        hi := mid;
      }
    }
    out := s[..lo] + [x] + s[lo..];
    assert forall y :: y in out <==> (y in s || y == x) by {
      forall y | y in out ensures y in s || y == x {
        var i :| 0 <= i < |out| && out[i] == y;
        if i < lo {
          assert s[i] == y;
        } else if i == lo {
          assert y == x;
        } else {
          assert s[i - 1] == y;
        }
      }
      forall y | y in s || y == x ensures y in out {
        if y == x {
          assert out[lo] == x;
        } else {
          var i :| 0 <= i < |s| && s[i] == y;
          if i < lo {
            assert out[i] == y;
          } else {
            assert out[i + 1] == y;
          }
        }
      }
    }
  }

  // Strictly sorted sequence of all IDs in `ids` using single-pass insertion
  // (avoids O(N^3) nested set-difference loops in compiled Go).
  method SortedPageIDs(ids: set<PageID>) returns (out: seq<PageID>)
    ensures StrictlySortedIDs(out)
    ensures |out| == |ids|
    ensures forall x :: x in out <==> x in ids
  {
    out := [];
    var rem := ids;
    while rem != {}
      invariant rem <= ids
      invariant StrictlySortedIDs(out)
      invariant |out| + |rem| == |ids|
      invariant forall x :: x in out <==> (x in ids && x !in rem)
      decreases |rem|
    {
      var x :| x in rem;
      out := InsertPageIDSorted(out, x);
      rem := rem - {x};
    }
  }
}
