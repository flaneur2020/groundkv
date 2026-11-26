// Package verified provides GroundKV's Dafny-verified embedded COW B+Tree
// storage engine implementation.
//
// It shares the exact on-disk binary format, Storage interface, Options, Meta,
// and error sentinels of package engine, while executing Z3-verified Dafny code
// translated to Go (under verified/dafnygen) for all binary page codecs,
// sorted B+Tree node operations, byte-aware multi-way splitting, MVCC freelist
// reclamation & sizing, dual-meta recovery selection, and 4-phase commit guards.
package verified

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVCodec"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVEngine"
	"github.com/flaneur2020/groundkv/verified/dafnygen/GroundKVTypes"
	_dafny "github.com/flaneur2020/groundkv/verified/dafnygen/dafny"
)

// Shared public types from package engine so callers can switch seamlessly
// between the pure-Go engine and the Dafny-verified engine.
type (
	PageID           = engine.PageID
	Options          = engine.Options
	Meta             = engine.Meta
	MetaSlot         = engine.MetaSlot
	FreelistSnapshot = engine.FreelistSnapshot
	Storage          = engine.Storage
	Tracer           = engine.Tracer
	TraceEvent       = engine.TraceEvent
)

func toDafnyHeader(h pageHeader) GroundKVTypes.PageHeader {
	return GroundKVTypes.Companion_PageHeader_.Create_PageHeader_(
		uint64(h.pgid),
		h.flags,
		h.count,
		h.checksum,
		h.txid,
	)
}

func fromDafnyHeader(h GroundKVTypes.PageHeader) pageHeader {
	return pageHeader{
		pgid:     PageID(h.Dtor_pgid()),
		flags:    h.Dtor_flags(),
		count:    h.Dtor_count(),
		checksum: h.Dtor_checksum(),
		txid:     h.Dtor_txid(),
	}
}

func toDafnyMeta(m Meta) GroundKVTypes.Meta {
	return GroundKVTypes.Companion_Meta_.Create_Meta_(
		m.Magic,
		m.Version,
		m.PageSize,
		uint64(m.Root),
		uint64(m.HighWater),
		m.TxID,
		uint64(m.Freelist),
	)
}

func fromDafnyMeta(m GroundKVTypes.Meta) Meta {
	return Meta{
		Magic:     m.Dtor_magic(),
		Version:   m.Dtor_version(),
		PageSize:  m.Dtor_pageSize(),
		Root:      PageID(m.Dtor_root()),
		HighWater: PageID(m.Dtor_highWater()),
		TxID:      m.Dtor_txid(),
		Freelist:  PageID(m.Dtor_freelist()),
	}
}

func toDafnyLeafEntries(entries []leafEntry) _dafny.Sequence {
	if len(entries) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(entries))
	for i, e := range entries {
		items[i] = GroundKVTypes.Companion_LeafEntry_.Create_LeafEntry_(
			_dafny.SeqOfBytes(e.key),
			_dafny.SeqOfBytes(e.value),
		)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyLeafEntries(seq _dafny.Sequence) []leafEntry {
	n := int(seq.Cardinality())
	out := make([]leafEntry, n)
	for i := 0; i < n; i++ {
		e := seq.Select(uint32(i)).(GroundKVTypes.LeafEntry)
		out[i] = leafEntry{
			key:   _dafny.ToByteArray(e.Dtor_key()),
			value: _dafny.ToByteArray(e.Dtor_value()),
		}
	}
	return out
}

func toDafnyBranchEntries(entries []branchEntry) _dafny.Sequence {
	if len(entries) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(entries))
	for i, e := range entries {
		items[i] = GroundKVTypes.Companion_BranchEntry_.Create_BranchEntry_(
			_dafny.SeqOfBytes(e.key),
			uint64(e.child),
		)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyBranchEntries(seq _dafny.Sequence) []branchEntry {
	n := int(seq.Cardinality())
	out := make([]branchEntry, n)
	for i := 0; i < n; i++ {
		e := seq.Select(uint32(i)).(GroundKVTypes.BranchEntry)
		out[i] = branchEntry{
			key:   _dafny.ToByteArray(e.Dtor_key()),
			child: PageID(e.Dtor_child()),
		}
	}
	return out
}

func toDafnyPageIDs(ids []PageID) _dafny.Sequence {
	if len(ids) == 0 {
		return _dafny.EmptySeq
	}
	items := make([]interface{}, len(ids))
	for i, id := range ids {
		items[i] = uint64(id)
	}
	return _dafny.SeqOf(items...)
}

func fromDafnyPageIDs(seq _dafny.Sequence) []PageID {
	n := int(seq.Cardinality())
	out := make([]PageID, n)
	for i := 0; i < n; i++ {
		out[i] = PageID(seq.Select(uint32(i)).(uint64))
	}
	return out
}

func toDafnyPageIDSet(ids []PageID) _dafny.Set {
	if len(ids) == 0 {
		return _dafny.EmptySet
	}
	sb := _dafny.NewBuilder()
	for _, id := range ids {
		sb.Add(uint64(id))
	}
	return sb.ToSet()
}

func fromDafnyErrCode(ec GroundKVTypes.ErrCode) error {
	switch {
	case ec.Is_ErrCorruptedMeta():
		return engine.ErrCorruptedMeta
	case ec.Is_ErrEmptyKey():
		return engine.ErrEmptyKey
	case ec.Is_ErrKeyTooLarge():
		return engine.ErrKeyTooLarge
	case ec.Is_ErrValueTooLarge():
		return engine.ErrValueTooLarge
	case ec.Is_ErrNotFound():
		return engine.ErrNotFound
	case ec.Is_ErrWriteTxActive():
		return engine.ErrWriteTxActive
	default:
		return engine.ErrCorruptedPage
	}
}

// stampPageChecksum stamps the CRC32-IEEE checksum at header offset [12:16]
// over a page buffer encoded by the verified Dafny page codec.
func stampPageChecksum(buf []byte) {
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	sum := crc32.ChecksumIEEE(buf)
	binary.LittleEndian.PutUint32(buf[12:16], sum)
}

func toDafnyCommitPhase(p txCommitPhase, closed bool) GroundKVEngine.CommitPhase {
	if closed {
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseIdle_()
	}
	switch p {
	case txPhaseActive:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseActive_()
	case txPhaseDataWritten:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataWritten_()
	case txPhaseDataSynced:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataSynced_()
	case txPhaseMetaWritten:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseMetaWritten_()
	default:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseIdle_()
	}
}

func selectRecoveredMeta(meta0 Meta, ok0 bool, meta1 Meta, ok1 bool) (Meta, error) {
	opt0 := GroundKVTypes.Companion_Option_.Create_None_()
	if ok0 {
		opt0 = GroundKVTypes.Companion_Option_.Create_Some_(toDafnyMeta(meta0))
	}
	opt1 := GroundKVTypes.Companion_Option_.Create_None_()
	if ok1 {
		opt1 = GroundKVTypes.Companion_Option_.Create_Some_(toDafnyMeta(meta1))
	}
	res := GroundKVEngine.Companion_Default___.SelectRecoveredMeta(opt0, opt1)
	if res.Is_Err() {
		return Meta{}, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return fromDafnyMeta(res.Dtor_value().(GroundKVTypes.Meta)), nil
}

func normalizeOptions(opts *Options) (Options, error) {
	var cfg Options
	if opts != nil {
		cfg = *opts
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = engine.DefaultPageSize
	}
	if cfg.PageSize < engine.MinPageSize || (cfg.PageSize&(cfg.PageSize-1)) != 0 {
		return Options{}, engine.ErrInvalidOptions
	}
	if cfg.MaxLeafKeys < 0 || cfg.MaxBranchKeys < 0 || cfg.MaxFreelistIDs < 0 {
		return Options{}, engine.ErrInvalidOptions
	}
	if cfg.MaxLeafKeys == 1 || cfg.MaxBranchKeys == 1 {
		return Options{}, engine.ErrInvalidOptions
	}
	if cfg.MaxFreelistIDs > 0 && cfg.MaxFreelistIDs < 3 {
		return Options{}, engine.ErrInvalidOptions
	}
	return cfg, nil
}

var _ = GroundKVCodec.Companion_Default___
