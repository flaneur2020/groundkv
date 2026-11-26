// GroundKV Phase 1 — Verified Binary Page Codecs (Little-Endian + Round-Trip Bijections)
// Matches the exact on-disk page formats in engine/page.go, engine/meta.go, engine/freelist.go.
// CRC32 checksum computation stays in the thin Go shell; header/payload layout, bounds safety,
// parity/highWater validation, and encode/decode bijections are machine-checked here.

include "Types.dfy"

module GroundKVCodec {
  import opened GroundKVTypes

  // =========================================================================
  // Little-endian fixed-width codecs with Z3-proved round-trip bijections
  // =========================================================================

  function EncodeU16LE(v: uint16): Bytes
    ensures |EncodeU16LE(v)| == 2
  {
    [
      (v % 0x100) as uint8,
      ((v / 0x100) % 0x100) as uint8
    ]
  }

  function DecodeU16LE(b: Bytes, off: nat): uint16
    requires off + 2 <= |b|
  {
    ((b[off] as int) + (b[off + 1] as int) * 0x100) as uint16
  }

  lemma RoundTripU16LE(v: uint16, prefix: Bytes, suffix: Bytes)
    ensures DecodeU16LE(prefix + EncodeU16LE(v) + suffix, |prefix|) == v
  {}

  function EncodeU32LE(v: uint32): Bytes
    ensures |EncodeU32LE(v)| == 4
  {
    [
      (v % 0x100) as uint8,
      ((v / 0x100) % 0x100) as uint8,
      ((v / 0x1_0000) % 0x100) as uint8,
      ((v / 0x100_0000) % 0x100) as uint8
    ]
  }

  function DecodeU32LE(b: Bytes, off: nat): uint32
    requires off + 4 <= |b|
  {
    ((b[off] as int)
      + (b[off + 1] as int) * 0x100
      + (b[off + 2] as int) * 0x1_0000
      + (b[off + 3] as int) * 0x100_0000) as uint32
  }

  lemma RoundTripU32LE(v: uint32, prefix: Bytes, suffix: Bytes)
    ensures DecodeU32LE(prefix + EncodeU32LE(v) + suffix, |prefix|) == v
  {}

  function EncodeU64LE(v: uint64): Bytes
    ensures |EncodeU64LE(v)| == 8
  {
    var lo: uint32 := (v % 0x1_0000_0000) as uint32;
    var hi: uint32 := (v / 0x1_0000_0000) as uint32;
    EncodeU32LE(lo) + EncodeU32LE(hi)
  }

  function DecodeU64LE(b: Bytes, off: nat): uint64
    requires off + 8 <= |b|
  {
    var lo := DecodeU32LE(b, off) as int;
    var hi := DecodeU32LE(b, off + 4) as int;
    (lo + hi * 0x1_0000_0000) as uint64
  }

  lemma RoundTripU64LE(v: uint64, prefix: Bytes, suffix: Bytes)
    ensures DecodeU64LE(prefix + EncodeU64LE(v) + suffix, |prefix|) == v
  {
    var lo: uint32 := (v % 0x1_0000_0000) as uint32;
    var hi: uint32 := (v / 0x1_0000_0000) as uint32;
    var full := prefix + EncodeU64LE(v) + suffix;
    assert full == prefix + EncodeU32LE(lo) + (EncodeU32LE(hi) + suffix);
    RoundTripU32LE(lo, prefix, EncodeU32LE(hi) + suffix);
    assert full == (prefix + EncodeU32LE(lo)) + EncodeU32LE(hi) + suffix;
    RoundTripU32LE(hi, prefix + EncodeU32LE(lo), suffix);
  }

  function ZeroBytes(n: nat): Bytes
    ensures |ZeroBytes(n)| == n
    ensures forall i | 0 <= i < n :: ZeroBytes(n)[i] == 0
  {
    seq(n, _ => 0 as uint8)
  }

  // =========================================================================
  // Page Header Codec (24 bytes)
  //   [0:8]   pgid     (uint64)
  //   [8:10]  flags    (uint16)
  //   [10:12] count    (uint16)
  //   [12:16] checksum (uint32)
  //   [16:24] txid     (uint64)
  // =========================================================================

  function EncodePageHeader(h: PageHeader): Bytes
    ensures |EncodePageHeader(h)| == 24
  {
    EncodeU64LE(h.pgid)
      + EncodeU16LE(h.flags)
      + EncodeU16LE(h.count)
      + EncodeU32LE(h.checksum)
      + EncodeU64LE(h.txid)
  }

  function DecodePageHeader(buf: Bytes): Result<PageHeader, ErrCode>
  {
    if |buf| < 24 then
      Err(ErrCorruptedPage)
    else
      Ok(PageHeader(
        DecodeU64LE(buf, 0),
        DecodeU16LE(buf, 8),
        DecodeU16LE(buf, 10),
        DecodeU32LE(buf, 12),
        DecodeU64LE(buf, 16)
      ))
  }

  lemma RoundTripPageHeader(h: PageHeader, suffix: Bytes)
    ensures DecodePageHeader(EncodePageHeader(h) + suffix) == Ok(h)
  {
    var b0 := EncodeU64LE(h.pgid);
    var b1 := EncodeU16LE(h.flags);
    var b2 := EncodeU16LE(h.count);
    var b3 := EncodeU32LE(h.checksum);
    var b4 := EncodeU64LE(h.txid);
    var full := EncodePageHeader(h) + suffix;
    assert full == [] + b0 + (b1 + b2 + b3 + b4 + suffix);
    RoundTripU64LE(h.pgid, [], b1 + b2 + b3 + b4 + suffix);
    assert full == b0 + b1 + (b2 + b3 + b4 + suffix);
    RoundTripU16LE(h.flags, b0, b2 + b3 + b4 + suffix);
    assert full == (b0 + b1) + b2 + (b3 + b4 + suffix);
    RoundTripU16LE(h.count, b0 + b1, b3 + b4 + suffix);
    assert full == (b0 + b1 + b2) + b3 + (b4 + suffix);
    RoundTripU32LE(h.checksum, b0 + b1 + b2, b4 + suffix);
    assert full == (b0 + b1 + b2 + b3) + b4 + suffix;
    RoundTripU64LE(h.txid, b0 + b1 + b2 + b3, suffix);
  }

  // =========================================================================
  // Dual-Meta Page Codec (48-byte payload at offset 24)
  // =========================================================================

  function MetaSlot(txid: uint64): PageID
    ensures MetaSlot(txid) == 0 || MetaSlot(txid) == 1
  {
    (txid % 2) as uint64
  }

  predicate ValidMeta(m: Meta, slot: PageID)
  {
    m.magic == MetaMagic &&
    m.version == MetaVersion &&
    (m.txid == 0 || MetaSlot(m.txid) == slot) &&
    m.highWater >= 2 &&
    (m.root == 0 || (2 <= m.root && m.root < m.highWater)) &&
    (m.freelist == 0 || (2 <= m.freelist && m.freelist < m.highWater))
  }

  function EncodeMetaPayload(m: Meta): Bytes
    ensures |EncodeMetaPayload(m)| == 48
  {
    EncodeU32LE(m.magic)
      + EncodeU32LE(m.version)
      + EncodeU32LE(m.pageSize)
      + EncodeU32LE(0)
      + EncodeU64LE(m.root)
      + EncodeU64LE(m.highWater)
      + EncodeU64LE(m.txid)
      + EncodeU64LE(m.freelist)
  }

  // Encodes a full pageSize-byte meta page (with checksum field set to 0 for
  // the Go shell to stamp CRC32-IEEE over the returned page).
  function EncodeMetaPage(pageSize: uint32, slot: PageID, m: Meta): Result<Bytes, ErrCode>
    ensures EncodeMetaPage(pageSize, slot, m).Ok? ==> |EncodeMetaPage(pageSize, slot, m).value| == pageSize as nat
  {
    if (pageSize as nat) < 72 then
      Err(ErrCorruptedMeta)
    else
      var hdr := EncodePageHeader(PageHeader(slot, PageFlagMeta, 1, 0, m.txid));
      var payload := EncodeMetaPayload(m);
      var pad := ZeroBytes((pageSize as nat) - 72);
      Ok(hdr + payload + pad)
  }

  function DecodeMetaPayload(buf: Bytes, slot: PageID, h: PageHeader): Result<Meta, ErrCode>
  {
    if h.flags != PageFlagMeta || |buf| < 72 then
      Err(ErrCorruptedMeta)
    else
      var m := Meta(
        DecodeU32LE(buf, 24),
        DecodeU32LE(buf, 28),
        DecodeU32LE(buf, 32),
        DecodeU64LE(buf, 40),
        DecodeU64LE(buf, 48),
        DecodeU64LE(buf, 56),
        DecodeU64LE(buf, 64)
      );
      if m.magic != MetaMagic || m.version != MetaVersion then
        Err(ErrCorruptedMeta)
      else if h.txid != m.txid then
        Err(ErrCorruptedMeta)
      else if m.txid > 0 && MetaSlot(m.txid) != slot then
        Err(ErrCorruptedMeta)
      else if m.highWater < 2 then
        Err(ErrCorruptedMeta)
      else if m.root != 0 && (m.root < 2 || m.root >= m.highWater) then
        Err(ErrCorruptedMeta)
      else if m.freelist != 0 && (m.freelist < 2 || m.freelist >= m.highWater) then
        Err(ErrCorruptedMeta)
      else
        Ok(m)
  }

  lemma RoundTripMetaPage(pageSize: uint32, slot: PageID, m: Meta)
    requires (pageSize as nat) >= 72
    requires ValidMeta(m, slot)
    ensures EncodeMetaPage(pageSize, slot, m).Ok?
    ensures var buf := EncodeMetaPage(pageSize, slot, m).value;
            var h := PageHeader(slot, PageFlagMeta, 1, 0, m.txid);
            DecodePageHeader(buf) == Ok(h) &&
            DecodeMetaPayload(buf, slot, h) == Ok(m)
  {
    var h := PageHeader(slot, PageFlagMeta, 1, 0, m.txid);
    var hdr := EncodePageHeader(h);
    var pad := ZeroBytes((pageSize as nat) - 72);
    var buf := hdr + EncodeMetaPayload(m) + pad;
    assert buf == EncodeMetaPage(pageSize, slot, m).value;
    RoundTripPageHeader(h, EncodeMetaPayload(m) + pad);

    var p0 := EncodeU32LE(m.magic);
    var p1 := EncodeU32LE(m.version);
    var p2 := EncodeU32LE(m.pageSize);
    var p3 := EncodeU32LE(0);
    var p4 := EncodeU64LE(m.root);
    var p5 := EncodeU64LE(m.highWater);
    var p6 := EncodeU64LE(m.txid);
    var p7 := EncodeU64LE(m.freelist);

    assert buf == hdr + p0 + (p1 + p2 + p3 + p4 + p5 + p6 + p7 + pad);
    RoundTripU32LE(m.magic, hdr, p1 + p2 + p3 + p4 + p5 + p6 + p7 + pad);
    assert buf == (hdr + p0) + p1 + (p2 + p3 + p4 + p5 + p6 + p7 + pad);
    RoundTripU32LE(m.version, hdr + p0, p2 + p3 + p4 + p5 + p6 + p7 + pad);
    assert buf == (hdr + p0 + p1) + p2 + (p3 + p4 + p5 + p6 + p7 + pad);
    RoundTripU32LE(m.pageSize, hdr + p0 + p1, p3 + p4 + p5 + p6 + p7 + pad);
    assert buf == (hdr + p0 + p1 + p2 + p3) + p4 + (p5 + p6 + p7 + pad);
    RoundTripU64LE(m.root, hdr + p0 + p1 + p2 + p3, p5 + p6 + p7 + pad);
    assert buf == (hdr + p0 + p1 + p2 + p3 + p4) + p5 + (p6 + p7 + pad);
    RoundTripU64LE(m.highWater, hdr + p0 + p1 + p2 + p3 + p4, p6 + p7 + pad);
    assert buf == (hdr + p0 + p1 + p2 + p3 + p4 + p5) + p6 + (p7 + pad);
    RoundTripU64LE(m.txid, hdr + p0 + p1 + p2 + p3 + p4 + p5, p7 + pad);
    assert buf == (hdr + p0 + p1 + p2 + p3 + p4 + p5 + p6) + p7 + pad;
    RoundTripU64LE(m.freelist, hdr + p0 + p1 + p2 + p3 + p4 + p5 + p6, pad);
  }

  // =========================================================================
  // Leaf Page Codec
  // =========================================================================

  function LeafEntriesPayloadSize(entries: seq<LeafEntry>): nat
    decreases |entries|
  {
    if |entries| == 0 then 0
    else 8 + |entries[0].key| + |entries[0].value| + LeafEntriesPayloadSize(entries[1..])
  }

  function LeafEncodedSize(entries: seq<LeafEntry>): nat
  {
    24 + LeafEntriesPayloadSize(entries)
  }

  lemma LeafEntriesPayloadSizeConcat(a: seq<LeafEntry>, b: seq<LeafEntry>)
    ensures LeafEntriesPayloadSize(a + b) == LeafEntriesPayloadSize(a) + LeafEntriesPayloadSize(b)
    decreases |a|
  {
    if |a| == 0 {
      assert a + b == b;
    } else {
      assert (a + b)[0] == a[0];
      assert (a + b)[1..] == a[1..] + b;
      LeafEntriesPayloadSizeConcat(a[1..], b);
    }
  }

  predicate ValidLeafEntryLengths(entries: seq<LeafEntry>)
  {
    forall i | 0 <= i < |entries| ::
      |entries[i].key| < 0x1_0000_0000 && |entries[i].value| < 0x1_0000_0000
  }

  function EncodeLeafEntry(e: LeafEntry): Bytes
    requires |e.key| < 0x1_0000_0000 && |e.value| < 0x1_0000_0000
    ensures |EncodeLeafEntry(e)| == 8 + |e.key| + |e.value|
  {
    EncodeU32LE(|e.key| as uint32) + EncodeU32LE(|e.value| as uint32) + e.key + e.value
  }

  function EncodeLeafEntries(entries: seq<LeafEntry>): Bytes
    requires ValidLeafEntryLengths(entries)
    ensures |EncodeLeafEntries(entries)| == LeafEntriesPayloadSize(entries)
    decreases |entries|
  {
    if |entries| == 0 then []
    else EncodeLeafEntry(entries[0]) + EncodeLeafEntries(entries[1..])
  }

  function EncodeLeafPage(pageSize: uint32, pgid: PageID, txid: uint64, entries: seq<LeafEntry>): Result<Bytes, ErrCode>
    ensures EncodeLeafPage(pageSize, pgid, txid, entries).Ok? ==>
      |EncodeLeafPage(pageSize, pgid, txid, entries).value| == pageSize as nat
  {
    var sz := LeafEncodedSize(entries);
    if sz > pageSize as nat || |entries| >= 0x1_0000 then
      Err(ErrValueTooLarge)
    else
      assert ValidLeafEntryLengths(entries) by {
        LeafSizeBoundsEntries(entries);
      }
      var hdr := EncodePageHeader(PageHeader(pgid, PageFlagLeaf, |entries| as uint16, 0, txid));
      var body := EncodeLeafEntries(entries);
      var pad := ZeroBytes((pageSize as nat) - sz);
      Ok(hdr + body + pad)
  }

  lemma LeafSizeBoundsEntries(entries: seq<LeafEntry>)
    ensures forall i | 0 <= i < |entries| ::
      8 + |entries[i].key| + |entries[i].value| <= LeafEntriesPayloadSize(entries)
    decreases |entries|
  {
    if |entries| > 0 {
      LeafSizeBoundsEntries(entries[1..]);
    }
  }

  function DecodeLeafEntriesAt(buf: Bytes, off: nat, remaining: nat): Result<seq<LeafEntry>, ErrCode>
    requires off <= |buf|
    decreases remaining
  {
    if remaining == 0 then
      Ok([])
    else if off + 8 > |buf| then
      Err(ErrCorruptedPage)
    else
      var klen := DecodeU32LE(buf, off) as nat;
      var vlen := DecodeU32LE(buf, off + 4) as nat;
      var keyStart := off + 8;
      if keyStart + klen + vlen > |buf| then
        Err(ErrCorruptedPage)
      else
        var k := buf[keyStart .. keyStart + klen];
        var v := buf[keyStart + klen .. keyStart + klen + vlen];
        match DecodeLeafEntriesAt(buf, keyStart + klen + vlen, remaining - 1)
          case Err(e) => Err(e)
          case Ok(rest) => Ok([LeafEntry(k, v)] + rest)
  }

  function DecodeLeafPage(buf: Bytes, h: PageHeader): Result<seq<LeafEntry>, ErrCode>
  {
    if h.flags != PageFlagLeaf || |buf| < 24 then
      Err(ErrCorruptedPage)
    else
      DecodeLeafEntriesAt(buf, 24, h.count as nat)
  }

  lemma DecodeU32LEFromSlice(b: Bytes, off: nat, v: uint32)
    requires off + 4 <= |b|
    requires b[off .. off + 4] == EncodeU32LE(v)
    ensures DecodeU32LE(b, off) == v
  {
    var enc := EncodeU32LE(v);
    assert b[off] == enc[0] && b[off + 1] == enc[1] && b[off + 2] == enc[2] && b[off + 3] == enc[3];
  }

  lemma DecodeU64LEFromSlice(b: Bytes, off: nat, v: uint64)
    requires off + 8 <= |b|
    requires b[off .. off + 8] == EncodeU64LE(v)
    ensures DecodeU64LE(b, off) == v
  {
    var lo: uint32 := (v % 0x1_0000_0000) as uint32;
    var hi: uint32 := (v / 0x1_0000_0000) as uint32;
    var enc := EncodeU64LE(v);
    assert enc == EncodeU32LE(lo) + EncodeU32LE(hi);
    assert enc[0..4] == EncodeU32LE(lo);
    assert enc[4..8] == EncodeU32LE(hi);
    assert b[off .. off + 4] == enc[0..4];
    assert b[off + 4 .. off + 8] == enc[4..8];
    DecodeU32LEFromSlice(b, off, lo);
    DecodeU32LEFromSlice(b, off + 4, hi);
  }

  lemma RoundTripLeafEntries(entries: seq<LeafEntry>, prefix: Bytes, suffix: Bytes)
    requires ValidLeafEntryLengths(entries)
    ensures DecodeLeafEntriesAt(prefix + EncodeLeafEntries(entries) + suffix, |prefix|, |entries|) == Ok(entries)
    decreases |entries|
  {
    if |entries| == 0 {
    } else {
      var e := entries[0];
      var rest := entries[1..];
      var entryBytes := EncodeLeafEntry(e);
      var restBytes := EncodeLeafEntries(rest);
      var full := prefix + EncodeLeafEntries(entries) + suffix;
      var off := |prefix|;
      assert full[off .. off + 4] == EncodeU32LE(|e.key| as uint32);
      assert full[off + 4 .. off + 8] == EncodeU32LE(|e.value| as uint32);
      DecodeU32LEFromSlice(full, off, |e.key| as uint32);
      DecodeU32LEFromSlice(full, off + 4, |e.value| as uint32);
      assert full[off + 8 .. off + 8 + |e.key|] == e.key;
      assert full[off + 8 + |e.key| .. off + 8 + |e.key| + |e.value|] == e.value;
      var newPrefix := prefix + entryBytes;
      assert |newPrefix| == off + 8 + |e.key| + |e.value|;
      assert full == newPrefix + restBytes + suffix;
      RoundTripLeafEntries(rest, newPrefix, suffix);
      assert [e] + rest == entries;
    }
  }

  lemma RoundTripLeafPage(pageSize: uint32, pgid: PageID, txid: uint64, entries: seq<LeafEntry>)
    requires LeafEncodedSize(entries) <= pageSize as nat
    requires |entries| < 0x1_0000
    ensures EncodeLeafPage(pageSize, pgid, txid, entries).Ok?
    ensures var buf := EncodeLeafPage(pageSize, pgid, txid, entries).value;
            var h := PageHeader(pgid, PageFlagLeaf, |entries| as uint16, 0, txid);
            DecodePageHeader(buf) == Ok(h) &&
            DecodeLeafPage(buf, h) == Ok(entries)
  {
    LeafSizeBoundsEntries(entries);
    var h := PageHeader(pgid, PageFlagLeaf, |entries| as uint16, 0, txid);
    var hdr := EncodePageHeader(h);
    var body := EncodeLeafEntries(entries);
    var pad := ZeroBytes((pageSize as nat) - LeafEncodedSize(entries));
    RoundTripPageHeader(h, body + pad);
    RoundTripLeafEntries(entries, hdr, pad);
  }

  // =========================================================================
  // Branch Page Codec
  // =========================================================================

  function BranchEntriesPayloadSize(entries: seq<BranchEntry>): nat
    decreases |entries|
  {
    if |entries| == 0 then 0
    else 12 + |entries[0].key| + BranchEntriesPayloadSize(entries[1..])
  }

  function BranchEncodedSize(entries: seq<BranchEntry>): nat
  {
    24 + BranchEntriesPayloadSize(entries)
  }

  lemma BranchEntriesPayloadSizeConcat(a: seq<BranchEntry>, b: seq<BranchEntry>)
    ensures BranchEntriesPayloadSize(a + b) == BranchEntriesPayloadSize(a) + BranchEntriesPayloadSize(b)
    decreases |a|
  {
    if |a| == 0 {
      assert a + b == b;
    } else {
      assert (a + b)[0] == a[0];
      assert (a + b)[1..] == a[1..] + b;
      BranchEntriesPayloadSizeConcat(a[1..], b);
    }
  }

  predicate ValidBranchEntryLengths(entries: seq<BranchEntry>)
  {
    forall i | 0 <= i < |entries| :: |entries[i].key| < 0x1_0000_0000
  }

  lemma BranchSizeBoundsEntries(entries: seq<BranchEntry>)
    ensures forall i | 0 <= i < |entries| ::
      12 + |entries[i].key| <= BranchEntriesPayloadSize(entries)
    decreases |entries|
  {
    if |entries| > 0 {
      BranchSizeBoundsEntries(entries[1..]);
    }
  }

  function EncodeBranchEntry(e: BranchEntry): Bytes
    requires |e.key| < 0x1_0000_0000
    ensures |EncodeBranchEntry(e)| == 12 + |e.key|
  {
    EncodeU32LE(|e.key| as uint32) + EncodeU64LE(e.child) + e.key
  }

  function EncodeBranchEntries(entries: seq<BranchEntry>): Bytes
    requires ValidBranchEntryLengths(entries)
    ensures |EncodeBranchEntries(entries)| == BranchEntriesPayloadSize(entries)
    decreases |entries|
  {
    if |entries| == 0 then []
    else EncodeBranchEntry(entries[0]) + EncodeBranchEntries(entries[1..])
  }

  function EncodeBranchPage(pageSize: uint32, pgid: PageID, txid: uint64, entries: seq<BranchEntry>): Result<Bytes, ErrCode>
    ensures EncodeBranchPage(pageSize, pgid, txid, entries).Ok? ==>
      |EncodeBranchPage(pageSize, pgid, txid, entries).value| == pageSize as nat
  {
    var sz := BranchEncodedSize(entries);
    if sz > pageSize as nat || |entries| >= 0x1_0000 then
      Err(ErrKeyTooLarge)
    else
      assert ValidBranchEntryLengths(entries) by {
        BranchSizeBoundsEntries(entries);
      }
      var hdr := EncodePageHeader(PageHeader(pgid, PageFlagBranch, |entries| as uint16, 0, txid));
      var body := EncodeBranchEntries(entries);
      var pad := ZeroBytes((pageSize as nat) - sz);
      Ok(hdr + body + pad)
  }

  function DecodeBranchEntriesAt(buf: Bytes, off: nat, remaining: nat): Result<seq<BranchEntry>, ErrCode>
    requires off <= |buf|
    decreases remaining
  {
    if remaining == 0 then
      Ok([])
    else if off + 12 > |buf| then
      Err(ErrCorruptedPage)
    else
      var klen := DecodeU32LE(buf, off) as nat;
      var child := DecodeU64LE(buf, off + 4);
      var keyStart := off + 12;
      if keyStart + klen > |buf| then
        Err(ErrCorruptedPage)
      else
        var k := buf[keyStart .. keyStart + klen];
        match DecodeBranchEntriesAt(buf, keyStart + klen, remaining - 1)
          case Err(e) => Err(e)
          case Ok(rest) => Ok([BranchEntry(k, child)] + rest)
  }

  function DecodeBranchPage(buf: Bytes, h: PageHeader): Result<seq<BranchEntry>, ErrCode>
  {
    if h.flags != PageFlagBranch || |buf| < 24 then
      Err(ErrCorruptedPage)
    else
      DecodeBranchEntriesAt(buf, 24, h.count as nat)
  }

  lemma RoundTripBranchEntries(entries: seq<BranchEntry>, prefix: Bytes, suffix: Bytes)
    requires ValidBranchEntryLengths(entries)
    ensures DecodeBranchEntriesAt(prefix + EncodeBranchEntries(entries) + suffix, |prefix|, |entries|) == Ok(entries)
    decreases |entries|
  {
    if |entries| == 0 {
    } else {
      var e := entries[0];
      var rest := entries[1..];
      var entryBytes := EncodeBranchEntry(e);
      var restBytes := EncodeBranchEntries(rest);
      var full := prefix + EncodeBranchEntries(entries) + suffix;
      var off := |prefix|;
      assert full[off .. off + 4] == EncodeU32LE(|e.key| as uint32);
      assert full[off + 4 .. off + 12] == EncodeU64LE(e.child);
      DecodeU32LEFromSlice(full, off, |e.key| as uint32);
      DecodeU64LEFromSlice(full, off + 4, e.child);
      assert full[off + 12 .. off + 12 + |e.key|] == e.key;
      var newPrefix := prefix + entryBytes;
      assert |newPrefix| == off + 12 + |e.key|;
      assert full == newPrefix + restBytes + suffix;
      RoundTripBranchEntries(rest, newPrefix, suffix);
      assert [e] + rest == entries;
    }
  }

  lemma RoundTripBranchPage(pageSize: uint32, pgid: PageID, txid: uint64, entries: seq<BranchEntry>)
    requires BranchEncodedSize(entries) <= pageSize as nat
    requires |entries| < 0x1_0000
    ensures EncodeBranchPage(pageSize, pgid, txid, entries).Ok?
    ensures var buf := EncodeBranchPage(pageSize, pgid, txid, entries).value;
            var h := PageHeader(pgid, PageFlagBranch, |entries| as uint16, 0, txid);
            DecodePageHeader(buf) == Ok(h) &&
            DecodeBranchPage(buf, h) == Ok(entries)
  {
    BranchSizeBoundsEntries(entries);
    var h := PageHeader(pgid, PageFlagBranch, |entries| as uint16, 0, txid);
    var hdr := EncodePageHeader(h);
    var body := EncodeBranchEntries(entries);
    var pad := ZeroBytes((pageSize as nat) - BranchEncodedSize(entries));
    RoundTripPageHeader(h, body + pad);
    RoundTripBranchEntries(entries, hdr, pad);
  }

  // =========================================================================
  // Freelist Page Codec
  // =========================================================================

  function MaxFreelistIDsPerPage(pageSize: uint32): nat
  {
    if (pageSize as nat) < 32 then 0
    else ((pageSize as nat) - 32) / 8
  }

  function EncodePageIDs(ids: seq<PageID>): Bytes
    ensures |EncodePageIDs(ids)| == |ids| * 8
    decreases |ids|
  {
    if |ids| == 0 then []
    else EncodeU64LE(ids[0]) + EncodePageIDs(ids[1..])
  }

  function EncodeFreelistPage(pageSize: uint32, pgid: PageID, txid: uint64, next: PageID, ids: seq<PageID>): Result<Bytes, ErrCode>
    ensures EncodeFreelistPage(pageSize, pgid, txid, next, ids).Ok? ==>
      |EncodeFreelistPage(pageSize, pgid, txid, next, ids).value| == pageSize as nat
  {
    if (pageSize as nat) < 32 || |ids| > MaxFreelistIDsPerPage(pageSize) || |ids| >= 0x1_0000 then
      Err(ErrValueTooLarge)
    else
      var hdr := EncodePageHeader(PageHeader(pgid, PageFlagFreelist, |ids| as uint16, 0, txid));
      var nextBytes := EncodeU64LE(next);
      var idBytes := EncodePageIDs(ids);
      var pad := ZeroBytes((pageSize as nat) - 32 - |ids| * 8);
      Ok(hdr + nextBytes + idBytes + pad)
  }

  function DecodePageIDsAt(buf: Bytes, off: nat, count: nat): seq<PageID>
    requires off + count * 8 <= |buf|
    ensures |DecodePageIDsAt(buf, off, count)| == count
    decreases count
  {
    if count == 0 then []
    else [DecodeU64LE(buf, off)] + DecodePageIDsAt(buf, off + 8, count - 1)
  }

  datatype DecodedFreelist = DecodedFreelist(next: PageID, ids: seq<PageID>)

  function DecodeFreelistPage(buf: Bytes, h: PageHeader): Result<DecodedFreelist, ErrCode>
  {
    if h.flags != PageFlagFreelist || |buf| < 32 + (h.count as nat) * 8 then
      Err(ErrCorruptedPage)
    else
      Ok(DecodedFreelist(
        DecodeU64LE(buf, 24),
        DecodePageIDsAt(buf, 32, h.count as nat)
      ))
  }

  lemma RoundTripPageIDs(ids: seq<PageID>, prefix: Bytes, suffix: Bytes)
    ensures DecodePageIDsAt(prefix + EncodePageIDs(ids) + suffix, |prefix|, |ids|) == ids
    decreases |ids|
  {
    if |ids| > 0 {
      var id0 := EncodeU64LE(ids[0]);
      var rest := EncodePageIDs(ids[1..]);
      var full := prefix + EncodePageIDs(ids) + suffix;
      assert full == prefix + id0 + (rest + suffix);
      RoundTripU64LE(ids[0], prefix, rest + suffix);
      assert full == (prefix + id0) + rest + suffix;
      RoundTripPageIDs(ids[1..], prefix + id0, suffix);
    }
  }

  lemma RoundTripFreelistPage(pageSize: uint32, pgid: PageID, txid: uint64, next: PageID, ids: seq<PageID>)
    requires (pageSize as nat) >= 32
    requires |ids| <= MaxFreelistIDsPerPage(pageSize)
    requires |ids| < 0x1_0000
    ensures EncodeFreelistPage(pageSize, pgid, txid, next, ids).Ok?
    ensures var buf := EncodeFreelistPage(pageSize, pgid, txid, next, ids).value;
            var h := PageHeader(pgid, PageFlagFreelist, |ids| as uint16, 0, txid);
            DecodePageHeader(buf) == Ok(h) &&
            DecodeFreelistPage(buf, h) == Ok(DecodedFreelist(next, ids))
  {
    var h := PageHeader(pgid, PageFlagFreelist, |ids| as uint16, 0, txid);
    var hdr := EncodePageHeader(h);
    var nextBytes := EncodeU64LE(next);
    var idBytes := EncodePageIDs(ids);
    var pad := ZeroBytes((pageSize as nat) - 32 - |ids| * 8);
    var buf := hdr + nextBytes + idBytes + pad;
    assert buf == EncodeFreelistPage(pageSize, pgid, txid, next, ids).value;
    RoundTripPageHeader(h, nextBytes + idBytes + pad);
    assert buf == hdr + nextBytes + (idBytes + pad);
    RoundTripU64LE(next, hdr, idBytes + pad);
    assert buf == (hdr + nextBytes) + idBytes + pad;
    RoundTripPageIDs(ids, hdr + nextBytes, pad);
  }
}
