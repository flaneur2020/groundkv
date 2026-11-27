// Package verified provides GroundKV's Dafny-verified storage kernel
// (`engine.Core`) backed by Z3-verified Dafny code translated to Go under
// engine/verified/internal/dafnygen.
//
// Each domain sub-interface of engine.Core is implemented in its matching file:
//   - page.go:     engine.PageCodec    (GroundKVCodec header/leaf/branch)
//   - meta.go:     engine.MetaCore     (GroundKVCodec meta + GroundKVEngine recovery)
//   - btree.go:    engine.BTreeCore    (GroundKVNode lookup/mutation/splitting)
//   - freelist.go: engine.FreelistCore & engine.FreelistPool (GroundKVFreelist + codec)
//   - tx.go:       engine.CommitCore   (GroundKVEngine 4-phase transition guards)
//
// Call verified.Open / verified.OpenStorage (or pass verified.NewCore() via
// engine.Options.Core) to execute the verified kernel inside GroundKV's shared
// I/O, locking, and crash-fencing shell.
package verified

import (
	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVTypes"
)

type dafnyCore struct {
	dafnyPageCodec
	dafnyMetaCore
	dafnyBTreeCore
	dafnyFreelistCore
	dafnyCommitCore
}

var (
	_ engine.Core = dafnyCore{}

	defaultDafnyCore engine.Core = dafnyCore{}
)

// NewCore returns GroundKV's Z3-verified Dafny-compiled engine.Core.
func NewCore() engine.Core { return defaultDafnyCore }

// Open opens or initializes a GroundKV database backed by the Dafny-verified Core.
func Open(path string, opts *engine.Options) (*engine.DB, error) {
	return engine.Open(path, withVerifiedCore(opts))
}

// OpenStorage opens or initializes a GroundKV database on st backed by the Dafny-verified Core.
func OpenStorage(st engine.Storage, opts *engine.Options) (*engine.DB, error) {
	return engine.OpenStorage(st, withVerifiedCore(opts))
}

func withVerifiedCore(opts *engine.Options) *engine.Options {
	var cfg engine.Options
	if opts != nil {
		cfg = *opts
	}
	cfg.Core = defaultDafnyCore
	return &cfg
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
