package engine

import (
	"encoding/json"
	"io"
	"sync"
)

// TraceEvent captures a single atomic storage-protocol transition emitted by
// GroundKV for formal trace validation (e.g., TLA+ / TLC trace checking).
type TraceEvent struct {
	Event         string   `json:"event"`
	TxID          uint64   `json:"txid"`
	Phase         string   `json:"phase"`
	Root          PageID   `json:"root"`
	Freelist      PageID   `json:"freelist"`
	HighWater     PageID   `json:"high_water"`
	DirtyAltMeta  bool     `json:"dirty_alt_meta"`
	ActiveReaders int      `json:"active_readers"`
	MinReaderTxID int64    `json:"min_reader_txid"` // -1 when ActiveReaders == 0
	FreeCount     int      `json:"free_count"`
	PendingTxIDs  []uint64 `json:"pending_txids"`
	ReleaseBound  uint64   `json:"release_bound"`
}

// Tracer receives protocol transition events emitted by DB and Tx.
// Implementations must be safe for concurrent use if DB is shared.
type Tracer interface {
	Trace(ev TraceEvent)
}

// JSONLineTracer writes TraceEvents as newline-delimited JSON (NDJSON) suitable
// for direct consumption by TLA+ Trace Validation (Json!ndJsonDeserialize).
type JSONLineTracer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONLineTracer constructs a Tracer that writes one JSON object per line to w.
func NewJSONLineTracer(w io.Writer) *JSONLineTracer {
	return &JSONLineTracer{enc: json.NewEncoder(w)}
}

// Trace encodes ev as a single NDJSON line.
func (t *JSONLineTracer) Trace(ev TraceEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.enc.Encode(ev)
}

func (db *DB) emitTraceLocked(event string) {
	if db.opts.Tracer == nil {
		return
	}
	phase := "IDLE"
	if db.writeTx != nil {
		phase = db.writeTx.CommitPhase()
	}
	activeReaders := 0
	for _, c := range db.readerCounts {
		if c > 0 {
			activeReaders += c
		}
	}
	minReader := int64(-1)
	if r, ok := db.minActiveReaderTxIDLocked(); ok {
		minReader = int64(r)
	}
	pending := db.freelist.pendingTxIDs()
	if pending == nil {
		pending = []uint64{}
	}
	db.opts.Tracer.Trace(TraceEvent{
		Event:         event,
		TxID:          db.meta.TxID,
		Phase:         phase,
		Root:          db.meta.Root,
		Freelist:      db.meta.Freelist,
		HighWater:     db.meta.HighWater,
		DirtyAltMeta:  db.dirtyAltMeta,
		ActiveReaders: activeReaders,
		MinReaderTxID: minReader,
		FreeCount:     len(db.freelist.free),
		PendingTxIDs:  pending,
		ReleaseBound:  db.releaseBoundLocked(),
	})
}
