--------------------------- MODULE GroundKVTrace ----------------------------
(*
 * Formal TLA+ Trace Validation module for GroundKV.
 *
 * Loads a real NDJSON execution trace emitted by GroundKV's Go engine
 * (via engine.JSONLineTracer during DST swarm & crash runs) and verifies with
 * TLC that EVERY recorded step refines GroundKVProtocol!Next and preserves all
 * formal invariants (ValidMetaBounds, ReleaseBoundSafe, ReaderWatermarkValid,
 * CommittedTxIDMonotonic, NoDirtyAltMetaAfterDataSync).
 *
 * Run via:
 *   TRACE_PATH=/path/to/trace.ndjson java -cp tla2tools.jar:CommunityModules-deps.jar \
 *     tlc2.TLC -config formal/tla/GroundKVTrace.cfg formal/tla/GroundKVTrace.tla
 *)
EXTENDS GroundKVProtocol, Sequences, Json, IOUtils, TLC

VARIABLE l  \* 1-based cursor into the recorded NDJSON trace

traceVars == <<vars, l>>

TracePath ==
    IF "TRACE_PATH" \in DOMAIN IOEnv
    THEN IOEnv.TRACE_PATH
    ELSE "trace.ndjson"

Trace == ndJsonDeserialize(TracePath)

SeqToSet(seq) == { seq[i] : i \in 1..Len(seq) }

\* Assert that the formal TLA+ post-state matches every observed field from Go!
MatchesObserved(ev) ==
    /\ txid' = ev.txid
    /\ phase' = ev.phase
    /\ root' = ev.root
    /\ freelist' = ev.freelist
    /\ highWater' = ev.high_water
    /\ dirtyAltMeta' = ev.dirty_alt_meta
    /\ activeReaders' = ev.active_readers
    /\ minReaderTxID' = ev.min_reader_txid
    /\ pendingTxIDs' = SeqToSet(ev.pending_txids)
    /\ ev.release_bound = ReleaseBound(txid', activeReaders', minReaderTxID')

TraceInit ==
    /\ Len(Trace) >= 1
    /\ Trace[1].event = "Open"
    /\ txid = Trace[1].txid
    /\ phase = Trace[1].phase
    /\ root = Trace[1].root
    /\ freelist = Trace[1].freelist
    /\ highWater = Trace[1].high_water
    /\ dirtyAltMeta = Trace[1].dirty_alt_meta
    /\ activeReaders = Trace[1].active_readers
    /\ minReaderTxID = Trace[1].min_reader_txid
    /\ pendingTxIDs = SeqToSet(Trace[1].pending_txids)
    /\ lastSyncedTxID = Trace[1].txid
    /\ l = 2

TraceStep ==
    /\ l <= Len(Trace)
    /\ LET ev == Trace[l]
       IN /\ CASE ev.event = "BeginRead" ->
                      BeginReadStep
               [] ev.event = "CloseRead" ->
                      CloseReadStep(ev.min_reader_txid)
               [] ev.event = "BeginWrite" ->
                      BeginWriteStep
               [] ev.event = "WriteDataPages" ->
                      WriteDataPagesStep
               [] ev.event = "SyncDataPages" ->
                      SyncDataPagesStep
               [] ev.event = "WriteMetaPage" ->
                      WriteMetaPageStep
               [] ev.event = "SyncMetaPage" ->
                      SyncMetaPageStep(ev.root, ev.freelist, ev.high_water, SeqToSet(ev.pending_txids))
               [] ev.event = "RollbackWrite" ->
                      RollbackWriteStep(ev.high_water, ev.dirty_alt_meta, SeqToSet(ev.pending_txids))
               [] ev.event = "Open" ->
                      CrashRecoverStep(ev.txid, ev.root, ev.freelist, ev.high_water, SeqToSet(ev.pending_txids))
               [] OTHER ->
                      FALSE
          /\ MatchesObserved(ev)
    /\ l' = l + 1

\* Allow clean stuttering once every event in the trace has been validated.
TraceDone ==
    /\ l = Len(Trace) + 1
    /\ UNCHANGED traceVars

TraceNext ==
    TraceStep \/ TraceDone

TraceSpec ==
    TraceInit /\ [][TraceNext]_traceVars

\* Postcondition: TLC must validate every single event in the trace (diameter = Len(Trace)).
\* If any recorded Go transition violates GroundKVProtocol, TraceStep has no
\* successor before l = Len(Trace) + 1 and TLC immediately reports deadlock at that line!
TraceAccepted ==
    TLCGet("stats").diameter = Len(Trace)

=============================================================================
