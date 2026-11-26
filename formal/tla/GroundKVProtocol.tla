-------------------------- MODULE GroundKVProtocol --------------------------
(*
 * Unbounded formal TLA+ specification of GroundKV's dual-meta COW storage
 * protocol, MVCC reader-aware freelist reclamation watermark, POSIX FailSync
 * dirty-alternate-meta fencing, and crash recovery.
 *
 * Serves two complementary roles:
 *   1. Direct TLC model checking (GroundKVProtocol_MC.cfg) of safety invariants.
 *   2. Formal Trace Validation (GroundKVTrace.tla), checking real unbounded
 *      execution traces emitted by GroundKV's Go engine during DST & crash runs!
 *)
EXTENDS Integers, FiniteSets

CONSTANTS
    MaxTxID,       \* Used only by bounded model-checking config (unbounded in Trace Validation)
    MaxHighWater,  \* Used only by bounded model-checking config
    MaxReaders     \* Used only by bounded model-checking config

Phases == {"IDLE", "STAGED", "DATA_WRITTEN", "DATA_SYNCED", "META_WRITTEN"}

VARIABLES
    txid,           \* Active committed TxID (Nat)
    phase,          \* Active write transaction commit phase
    root,           \* Active B+Tree root PageID (0 when empty, else >= 2)
    freelist,       \* Active COW freelist chain head PageID (0 when empty, else >= 2)
    highWater,      \* Next append-only PageID (>= 2)
    dirtyAltMeta,   \* TRUE iff failed SyncMetaPage + failed Rollback neutralization left alt slot dirty
    activeReaders,  \* Number of currently open MVCC read transactions (Nat)
    minReaderTxID,  \* Lowest TxID among active readers, or -1 when activeReaders = 0
    pendingTxIDs,   \* Set of transaction IDs with unreleased freed pages in freelist.pending
    lastSyncedTxID  \* Highest durably committed TxID (durability watermark)

vars == <<
    txid, phase, root, freelist, highWater, dirtyAltMeta,
    activeReaders, minReaderTxID, pendingTxIDs, lastSyncedTxID
>>

Min(a, b) == IF a <= b THEN a ELSE b

\* Exact MVCC + dual-meta freelist reclamation watermark:
\*   releaseBound = min(max(0, txid - 1), minActiveReaderTxID)
ReleaseBound(curTx, readers, minR) ==
    LET base == IF curTx > 0 THEN curTx - 1 ELSE 0
    IN IF readers > 0 /\ minR < base THEN minR ELSE base

CurrentReleaseBound == ReleaseBound(txid, activeReaders, minReaderTxID)

Init ==
    /\ txid = 0
    /\ phase = "IDLE"
    /\ root = 0
    /\ freelist = 0
    /\ highWater = 2
    /\ dirtyAltMeta = FALSE
    /\ activeReaders = 0
    /\ minReaderTxID = -1
    /\ pendingTxIDs = {}
    /\ lastSyncedTxID = 0

\* --- Protocol Transitions (parameterized so both MC and Trace Validation share them) ---

BeginReadStep ==
    /\ activeReaders' = activeReaders + 1
    /\ minReaderTxID' = IF activeReaders = 0 THEN txid ELSE Min(minReaderTxID, txid)
    /\ UNCHANGED <<txid, phase, root, freelist, highWater, dirtyAltMeta, pendingTxIDs, lastSyncedTxID>>

CloseReadStep(nextMinR) ==
    /\ activeReaders > 0
    /\ activeReaders' = activeReaders - 1
    /\ IF activeReaders' = 0
       THEN nextMinR = -1
       ELSE nextMinR >= minReaderTxID /\ nextMinR <= txid
    /\ minReaderTxID' = nextMinR
    /\ LET bound == ReleaseBound(txid, activeReaders', nextMinR)
       IN pendingTxIDs' = {t \in pendingTxIDs : t > bound}
    /\ UNCHANGED <<txid, phase, root, freelist, highWater, dirtyAltMeta, lastSyncedTxID>>

BeginWriteStep ==
    /\ phase = "IDLE"
    /\ phase' = "STAGED"
    /\ pendingTxIDs' = {t \in pendingTxIDs : t > CurrentReleaseBound}
    /\ UNCHANGED <<txid, root, freelist, highWater, dirtyAltMeta, activeReaders, minReaderTxID, lastSyncedTxID>>

WriteDataPagesStep ==
    /\ phase = "STAGED"
    /\ phase' = "DATA_WRITTEN"
    /\ UNCHANGED <<txid, root, freelist, highWater, dirtyAltMeta, activeReaders, minReaderTxID, pendingTxIDs, lastSyncedTxID>>

\* SyncDataPages neutralizes any lingering dirtyAltMeta BEFORE calling file-wide fsync!
SyncDataPagesStep ==
    /\ phase = "DATA_WRITTEN"
    /\ phase' = "DATA_SYNCED"
    /\ dirtyAltMeta' = FALSE
    /\ UNCHANGED <<txid, root, freelist, highWater, activeReaders, minReaderTxID, pendingTxIDs, lastSyncedTxID>>

WriteMetaPageStep ==
    /\ phase = "DATA_SYNCED"
    /\ phase' = "META_WRITTEN"
    /\ UNCHANGED <<txid, root, freelist, highWater, dirtyAltMeta, activeReaders, minReaderTxID, pendingTxIDs, lastSyncedTxID>>

SyncMetaPageStep(newRoot, newFL, newHW, newPending) ==
    /\ phase = "META_WRITTEN"
    /\ txid' = txid + 1
    /\ lastSyncedTxID' = txid + 1
    /\ phase' = "IDLE"
    /\ dirtyAltMeta' = FALSE
    /\ newHW >= highWater
    /\ (newRoot = 0 \/ (newRoot >= 2 /\ newRoot < newHW))
    /\ (newFL = 0 \/ (newFL >= 2 /\ newFL < newHW))
    /\ root' = newRoot
    /\ freelist' = newFL
    /\ highWater' = newHW
    /\ LET nextBound == ReleaseBound(txid', activeReaders, minReaderTxID)
       IN /\ newPending \subseteq (pendingTxIDs \union {txid'})
          /\ \A t \in newPending : t > nextBound
          /\ pendingTxIDs' = newPending
    /\ UNCHANGED <<activeReaders, minReaderTxID>>

RollbackWriteStep(newHW, newDirtyAlt, newPending) ==
    /\ phase # "IDLE"
    /\ phase' = "IDLE"
    /\ IF phase = "META_WRITTEN"
       THEN \* Rollback after WriteMetaPage may advance HighWater, hold pages in
            \* pending[txid+1], and leave dirtyAltMeta=TRUE if zero-write failed.
            /\ newHW >= highWater
            /\ newDirtyAlt \in BOOLEAN
            /\ newPending \subseteq (pendingTxIDs \union {txid + 1})
            /\ \A t \in newPending : t > CurrentReleaseBound
       ELSE \* Rollback before WriteMetaPage never touches committed HighWater or pending.
            \* (A failed SyncDataPages may have cleared a prior dirtyAltMeta before failing fsync.)
            /\ newHW = highWater
            /\ (newDirtyAlt = dirtyAltMeta \/ (phase = "DATA_WRITTEN" /\ newDirtyAlt = FALSE))
            /\ newPending = pendingTxIDs
    /\ highWater' = newHW
    /\ dirtyAltMeta' = newDirtyAlt
    /\ pendingTxIDs' = newPending
    /\ UNCHANGED <<txid, root, freelist, activeReaders, minReaderTxID, lastSyncedTxID>>

\* Crash + Reopen recovery: enforces durability (never regress below lastSyncedTxID)
\* and strict crash atomicity (txid may advance by +1 ONLY if WriteMetaPage had run
\* or an un-neutralizable dirtyAltMeta was sitting in OS page cache!).
CrashRecoverStep(recTx, recRoot, recFL, recHW, recPending) ==
    /\ recTx >= lastSyncedTxID
    /\ IF phase = "META_WRITTEN" \/ dirtyAltMeta
       THEN recTx \in {txid, txid + 1}
       ELSE recTx = txid
    /\ recHW >= 2
    /\ (recRoot = 0 \/ (recRoot >= 2 /\ recRoot < recHW))
    /\ (recFL = 0 \/ (recFL >= 2 /\ recFL < recHW))
    /\ recPending \subseteq {recTx}
    /\ \A t \in recPending : t > ReleaseBound(recTx, 0, -1)
    /\ txid' = recTx
    /\ lastSyncedTxID' = recTx
    /\ phase' = "IDLE"
    /\ root' = recRoot
    /\ freelist' = recFL
    /\ highWater' = recHW
    /\ dirtyAltMeta' = FALSE
    /\ activeReaders' = 0
    /\ minReaderTxID' = -1
    /\ pendingTxIDs' = recPending

\* --- Bounded Next-State Relation for Direct TLC Model Checking ---

Next ==
    \/ (activeReaders < MaxReaders /\ BeginReadStep)
    \/ (\E r \in (-1)..txid : CloseReadStep(r))
    \/ (txid < MaxTxID /\ BeginWriteStep)
    \/ WriteDataPagesStep
    \/ SyncDataPagesStep
    \/ WriteMetaPageStep
    \/ (\E nr \in 0..MaxHighWater, nf \in 0..MaxHighWater, nhw \in highWater..MaxHighWater,
           np \in SUBSET (pendingTxIDs \union {txid + 1}) :
           SyncMetaPageStep(nr, nf, nhw, np))
    \/ (\E nhw \in highWater..MaxHighWater, nd \in BOOLEAN,
           np \in SUBSET (pendingTxIDs \union {txid + 1}) :
           RollbackWriteStep(nhw, nd, np))
    \/ (\E rt \in txid..(txid + 1) :
           \E rr \in 0..MaxHighWater, rf \in 0..MaxHighWater,
              rhw \in 2..MaxHighWater, rp \in SUBSET {rt} :
              CrashRecoverStep(rt, rr, rf, rhw, rp))

Spec == Init /\ [][Next]_vars

\* --- Formal Safety Invariants ---

TypeOK ==
    /\ txid \in Nat
    /\ phase \in Phases
    /\ root \in Nat
    /\ freelist \in Nat
    /\ highWater \in Nat
    /\ dirtyAltMeta \in BOOLEAN
    /\ activeReaders \in Nat
    /\ minReaderTxID \in Int
    /\ pendingTxIDs \subseteq Nat
    /\ lastSyncedTxID \in Nat

ValidMetaBounds ==
    /\ highWater >= 2
    /\ (root = 0 \/ (root >= 2 /\ root < highWater))
    /\ (freelist = 0 \/ (freelist >= 2 /\ freelist < highWater))

ReleaseBoundSafe ==
    \A t \in pendingTxIDs : t > CurrentReleaseBound

ReaderWatermarkValid ==
    /\ (activeReaders = 0 => minReaderTxID = -1)
    /\ (activeReaders > 0 => minReaderTxID >= 0 /\ minReaderTxID <= txid)

CommittedTxIDMonotonic ==
    txid >= lastSyncedTxID

NoDirtyAltMetaAfterDataSync ==
    (phase = "DATA_SYNCED" \/ phase = "META_WRITTEN") => ~dirtyAltMeta

=============================================================================
