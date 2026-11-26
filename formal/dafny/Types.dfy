// GroundKV Phase 1 — Core Types, Page Constants, and Lexicographical Byte Order
// Verified with Dafny 4.11 + Z3; compiled to Go via `dafny translate go`.

module GroundKVTypes {
  // Native fixed-width unsigned integer types that compile directly to Go
  // uint8, uint16, uint32, and uint64 without arbitrary-precision overhead.
  newtype {:nativeType "byte"} uint8 = x: int | 0 <= x < 0x100
  newtype {:nativeType "ushort"} uint16 = x: int | 0 <= x < 0x1_0000
  newtype {:nativeType "uint"} uint32 = x: int | 0 <= x < 0x1_0000_0000
  newtype {:nativeType "ulong"} uint64 = x: int | 0 <= x < 0x1_0000_0000_0000_0000

  type Bytes = seq<uint8>
  type PageID = uint64

  // On-disk page flags matching engine/page.go
  const PageFlagMeta: uint16 := 0x01
  const PageFlagBranch: uint16 := 0x02
  const PageFlagLeaf: uint16 := 0x04
  const PageFlagFreelist: uint16 := 0x08

  // On-disk byte sizes matching engine/page.go, engine/meta.go, engine/freelist.go
  const PageHeaderSize: uint32 := 24
  const LeafEntryHeaderSize: uint32 := 8
  const BranchEntryHeaderSize: uint32 := 12
  const MetaPayloadSize: uint32 := 48
  const FreelistHeaderExtra: uint32 := 8
  const FreelistEntrySize: uint32 := 8

  const MetaMagic: uint32 := 0x47524B56 // "GRKV"
  const MetaVersion: uint32 := 1
  const MinPageSize: uint32 := 256
  const MaxBTreeDepth: nat := 64

  datatype Option<T> = None | Some(value: T)

  datatype ErrCode =
    | ErrCorruptedPage
    | ErrCorruptedMeta
    | ErrEmptyKey
    | ErrKeyTooLarge
    | ErrValueTooLarge
    | ErrNotFound
    | ErrInvalidPhase
    | ErrWriteTxActive

  datatype Result<T, E> = Ok(value: T) | Err(error: E)

  datatype PageHeader = PageHeader(
    pgid: PageID,
    flags: uint16,
    count: uint16,
    checksum: uint32,
    txid: uint64
  )

  datatype Meta = Meta(
    magic: uint32,
    version: uint32,
    pageSize: uint32,
    root: PageID,
    highWater: PageID,
    txid: uint64,
    freelist: PageID
  )

  datatype LeafEntry = LeafEntry(key: Bytes, value: Bytes)
  datatype BranchEntry = BranchEntry(key: Bytes, child: PageID)

  // =========================================================================
  // Lexicographical byte-slice comparison (matches Go's bytes.Compare)
  // =========================================================================

  opaque function BytesCompare(a: Bytes, b: Bytes): int
    ensures BytesCompare(a, b) == -1 || BytesCompare(a, b) == 0 || BytesCompare(a, b) == 1
    decreases |a| + |b|
  {
    if |a| == 0 && |b| == 0 then 0
    else if |a| == 0 then -1
    else if |b| == 0 then 1
    else if a[0] < b[0] then -1
    else if a[0] > b[0] then 1
    else BytesCompare(a[1..], b[1..])
  }

  predicate BytesLt(a: Bytes, b: Bytes) {
    BytesCompare(a, b) < 0
  }

  predicate BytesLe(a: Bytes, b: Bytes) {
    BytesCompare(a, b) <= 0
  }

  // Strict total-order lemmas for BytesCompare

  lemma BytesCompareReflexive(a: Bytes)
    ensures BytesCompare(a, a) == 0
    decreases |a|
  {
    reveal BytesCompare();
    if |a| > 0 {
      BytesCompareReflexive(a[1..]);
    }
  }

  lemma BytesCompareEqIff(a: Bytes, b: Bytes)
    ensures BytesCompare(a, b) == 0 <==> a == b
    decreases |a| + |b|
  {
    reveal BytesCompare();
    if |a| == 0 && |b| == 0 {
    } else if |a| == 0 || |b| == 0 {
    } else if a[0] == b[0] {
      BytesCompareEqIff(a[1..], b[1..]);
      if a[1..] == b[1..] {
        assert a == [a[0]] + a[1..];
        assert b == [b[0]] + b[1..];
      }
    }
  }

  lemma BytesCompareAntisymmetric(a: Bytes, b: Bytes)
    ensures BytesCompare(a, b) == -BytesCompare(b, a)
    decreases |a| + |b|
  {
    reveal BytesCompare();
    if |a| > 0 && |b| > 0 && a[0] == b[0] {
      BytesCompareAntisymmetric(a[1..], b[1..]);
    }
  }

  lemma BytesCompareTransitive(a: Bytes, b: Bytes, c: Bytes)
    requires BytesLt(a, b)
    requires BytesLt(b, c)
    ensures BytesLt(a, c)
    decreases |a| + |b| + |c|
  {
    reveal BytesCompare();
    if |a| == 0 {
      assert |b| > 0;
      assert |c| > 0;
    } else {
      assert |b| > 0 && |c| > 0;
      if a[0] == b[0] && b[0] == c[0] {
        BytesCompareTransitive(a[1..], b[1..], c[1..]);
      }
    }
  }

  lemma BytesLeTransitive(a: Bytes, b: Bytes, c: Bytes)
    requires BytesLe(a, b)
    requires BytesLe(b, c)
    ensures BytesLe(a, c)
  {
    BytesCompareEqIff(a, b);
    BytesCompareEqIff(b, c);
    if BytesLt(a, b) && BytesLt(b, c) {
      BytesCompareTransitive(a, b, c);
    } else if a == b {
    } else {
      assert b == c;
    }
  }

  lemma BytesLtLeTransitive(a: Bytes, b: Bytes, c: Bytes)
    requires BytesLt(a, b)
    requires BytesLe(b, c)
    ensures BytesLt(a, c)
  {
    BytesCompareEqIff(b, c);
    if BytesLt(b, c) {
      BytesCompareTransitive(a, b, c);
    }
  }

  lemma BytesLeLtTransitive(a: Bytes, b: Bytes, c: Bytes)
    requires BytesLe(a, b)
    requires BytesLt(b, c)
    ensures BytesLt(a, c)
  {
    BytesCompareEqIff(a, b);
    if BytesLt(a, b) {
      BytesCompareTransitive(a, b, c);
    }
  }

  lemma BytesCompareTotal(a: Bytes, b: Bytes)
    ensures BytesLt(a, b) || a == b || BytesLt(b, a)
  {
    BytesCompareAntisymmetric(a, b);
    BytesCompareEqIff(a, b);
  }
}
