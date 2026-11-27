package engine

import (
	"github.com/flaneur2020/groundkv/engine/core"
	"github.com/flaneur2020/groundkv/engine/gocore"
)

// Public re-exports of engine/core types, constants, and interfaces so callers
// importing github.com/flaneur2020/groundkv/engine have a single ergonomic API.
type (
	PageID        = core.PageID
	PageHeader    = core.PageHeader
	Meta          = core.Meta
	LeafEntry     = core.LeafEntry
	BranchEntry   = core.BranchEntry
	CommitPhase   = core.CommitPhase
	Codec         = core.Codec
	BTreeNode     = core.BTreeNode
	FreelistPool  = core.FreelistPool
	Freelist      = core.Freelist
	Protocol      = core.Protocol
	Core          = core.Core
	CompositeCore = core.CompositeCore
)

const (
	PageFlagMeta     = core.PageFlagMeta
	PageFlagBranch   = core.PageFlagBranch
	PageFlagLeaf     = core.PageFlagLeaf
	PageFlagFreelist = core.PageFlagFreelist

	CommitPhaseIdle        = core.CommitPhaseIdle
	CommitPhaseActive      = core.CommitPhaseActive
	CommitPhaseDataWritten = core.CommitPhaseDataWritten
	CommitPhaseDataSynced  = core.CommitPhaseDataSynced
	CommitPhaseMetaWritten = core.CommitPhaseMetaWritten
)

// GoCore returns GroundKV's default pure-Go Core implementation.
func GoCore() Core { return gocore.New() }
