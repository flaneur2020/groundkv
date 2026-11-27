// Package verified provides GroundKV's Dafny-verified storage kernel
// (`engine/core.Core`) backed by Z3-verified Dafny code translated to Go under
// engine/verified/internal/dafnygen.
//
// Its file layout mirrors engine/gocore/ and formal/dafny/ 1:1:
//   - codec.go:    core.Codec     (GroundKVCodec: PageCodec.dfy)
//   - btree.go:    core.BTreeNode (GroundKVNode: BTreeNode.dfy)
//   - freelist.go: core.Freelist & core.FreelistPool (GroundKVFreelist: Freelist.dfy)
//   - protocol.go: core.Protocol  (GroundKVEngine: StorageEngine.dfy)
//
// Call verified.Open / verified.OpenStorage (or pass verified.NewCore() via
// engine.Options.Core) to execute the verified kernel inside GroundKV's shared
// I/O, locking, and crash-fencing shell.
package verified

import (
	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/core"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVTypes"
)

type dafnyCore struct {
	dafnyCodec
	dafnyBTreeNode
	dafnyFreelistCore
	dafnyProtocol
}

var (
	_ core.Core = dafnyCore{}

	defaultDafnyCore core.Core = dafnyCore{}
)

// NewCore returns GroundKV's Z3-verified Dafny-compiled core.Core.
func NewCore() core.Core { return defaultDafnyCore }

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
		return core.ErrCorruptedMeta
	case ec.Is_ErrEmptyKey():
		return core.ErrEmptyKey
	case ec.Is_ErrKeyTooLarge():
		return core.ErrKeyTooLarge
	case ec.Is_ErrValueTooLarge():
		return core.ErrValueTooLarge
	case ec.Is_ErrNotFound():
		return core.ErrNotFound
	case ec.Is_ErrWriteTxActive():
		return core.ErrWriteTxActive
	default:
		return core.ErrCorruptedPage
	}
}
