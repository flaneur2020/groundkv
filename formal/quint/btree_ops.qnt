// Ordered B+Tree & range-scan contract specification in Quint.
// Replayed as ITF Model-Based Testing traces against both engine.GoCore() and
// verified.NewCore() with MaxLeafKeys=2, MaxBranchKeys=2, MaxFreelistIDs=3
// (freelist enabled) to exercise 3-level branch splits, collapses, empty trees,
// multi-page freelist chains, and [start, end) range scans.
module btree_ops {
  pure val NOT_FOUND: str = "NOT_FOUND"
  pure val KEYS: List[str] = ["a", "b", "c", "d", "e"]
  pure val KEY_SET: Set[str] = Set("a", "b", "c", "d", "e")
  pure val RANGE_BOUNDS: Set[str] = Set("", "a", "b", "c", "d", "e", "f")

  type KV = {
    k: str,
    v: str,
  }

  type OpRecord = {
    op: str,
    key: str,
    startBound: str,
    endBound: str,
    result: str,
    scanResult: List[KV],
  }

  var txid: int
  var entries: str -> str
  var lastOp: OpRecord

  pure def keyOrder(k: str): int =
    if (k == "") 0
    else if (k == "a") 1
    else if (k == "b") 2
    else if (k == "c") 3
    else if (k == "d") 4
    else if (k == "e") 5
    else 6 // "f"

  pure def putVal(t: int): str =
    if (t == 1) "v1"
    else if (t == 2) "v2"
    else if (t == 3) "v3"
    else if (t == 4) "v4"
    else if (t == 5) "v5"
    else if (t == 6) "v6"
    else if (t == 7) "v7"
    else if (t == 8) "v8"
    else if (t == 9) "v9"
    else if (t == 10) "v10"
    else if (t == 11) "v11"
    else if (t == 12) "v12"
    else if (t == 13) "v13"
    else if (t == 14) "v14"
    else if (t == 15) "v15"
    else if (t == 16) "v16"
    else if (t == 17) "v17"
    else if (t == 18) "v18"
    else if (t == 19) "v19"
    else if (t == 20) "v20"
    else if (t == 21) "v21"
    else if (t == 22) "v22"
    else if (t == 23) "v23"
    else if (t == 24) "v24"
    else if (t == 25) "v25"
    else "vN"

  pure def bulkVal(t: int): str =
    if (t == 1) "b1"
    else if (t == 2) "b2"
    else if (t == 3) "b3"
    else if (t == 4) "b4"
    else if (t == 5) "b5"
    else if (t == 6) "b6"
    else if (t == 7) "b7"
    else if (t == 8) "b8"
    else if (t == 9) "b9"
    else if (t == 10) "b10"
    else if (t == 11) "b11"
    else if (t == 12) "b12"
    else if (t == 13) "b13"
    else if (t == 14) "b14"
    else if (t == 15) "b15"
    else if (t == 16) "b16"
    else if (t == 17) "b17"
    else if (t == 18) "b18"
    else if (t == 19) "b19"
    else if (t == 20) "b20"
    else if (t == 21) "b21"
    else if (t == 22) "b22"
    else if (t == 23) "b23"
    else if (t == 24) "b24"
    else if (t == 25) "b25"
    else "bN"

  pure def inRange(k: str, startB: str, endB: str): bool = {
    val loOK = startB == "" or keyOrder(k) >= keyOrder(startB)
    val hiOK = endB == "" or keyOrder(k) < keyOrder(endB)
    loOK and hiOK
  }

  pure def scanEntries(m: str -> str, startB: str, endB: str): List[KV] =
    KEYS.foldl([], (acc, k) =>
      if (m.keys().contains(k) and inRange(k, startB, endB))
        acc.append({ k: k, v: m.get(k) })
      else acc
    )

  action init = all {
    txid' = 0,
    entries' = Map(),
    lastOp' = { op: "Init", key: "", startBound: "", endBound: "", result: "OK", scanResult: [] },
  }

  action bulkLoad = all {
    txid < 24,
    val nextTx = txid + 1
    val tag = bulkVal(nextTx)
    val nextEntries = KEY_SET.mapBy(_ => tag)
    all {
      txid' = nextTx,
      entries' = nextEntries,
      lastOp' = { op: "BulkLoad", key: "", startBound: "", endBound: "", result: tag, scanResult: [] },
    },
  }

  action putKey(k: str): bool = all {
    txid < 24,
    val nextTx = txid + 1
    val v = putVal(nextTx)
    all {
      txid' = nextTx,
      entries' = entries.put(k, v),
      lastOp' = { op: "Put", key: k, startBound: "", endBound: "", result: v, scanResult: [] },
    },
  }

  action delKey(k: str): bool = all {
    txid < 24,
    val nextTx = txid + 1
    val kept = entries.keys().exclude(Set(k))
    all {
      txid' = nextTx,
      entries' = kept.mapBy(x => entries.get(x)),
      lastOp' = { op: "Delete", key: k, startBound: "", endBound: "", result: "OK", scanResult: [] },
    },
  }

  action getKey(k: str): bool = {
    val res = if (entries.keys().contains(k)) entries.get(k) else NOT_FOUND
    all {
      txid' = txid,
      entries' = entries,
      lastOp' = { op: "Get", key: k, startBound: "", endBound: "", result: res, scanResult: [] },
    }
  }

  action scanRange(startB: str, endB: str): bool = {
    val res = scanEntries(entries, startB, endB)
    all {
      txid' = txid,
      entries' = entries,
      lastOp' = { op: "ScanRange", key: "", startBound: startB, endBound: endB, result: "OK", scanResult: res },
    }
  }

  action step = any {
    bulkLoad,
    nondet k = KEY_SET.oneOf()
    putKey(k),
    nondet k = KEY_SET.oneOf()
    delKey(k),
    nondet k = KEY_SET.oneOf()
    getKey(k),
    nondet startB = RANGE_BOUNDS.oneOf()
    nondet endB = RANGE_BOUNDS.oneOf()
    scanRange(startB, endB),
  }

  val inv_bounded_key_domain: bool =
    entries.keys().subseteq(KEY_SET)

  val inv_txid_nonneg: bool =
    txid >= 0

  val inv_all: bool = inv_bounded_key_domain and inv_txid_nonneg

  run splitCollapseScanTest = {
    init
      .then(bulkLoad)
      .then(scanRange("b", "e"))
      .expect(lastOp.scanResult == [{ k: "b", v: "b1" }, { k: "c", v: "b1" }, { k: "d", v: "b1" }] and inv_all)
      .then(delKey("c"))
      .then(putKey("a"))
      .then(delKey("a"))
      .then(delKey("b"))
      .then(delKey("d"))
      .then(delKey("e"))
      .then(getKey("a"))
      .expect(entries.keys().size() == 0 and lastOp.result == NOT_FOUND and inv_all)
  }
}
