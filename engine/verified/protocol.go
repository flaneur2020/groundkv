package verified

import (
	"github.com/flaneur2020/groundkv/engine/core"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVEngine"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVTypes"
)

type dafnyProtocol struct{}

var _ core.Protocol = dafnyProtocol{}

func toDafnyCommitPhase(p core.CommitPhase) GroundKVEngine.CommitPhase {
	switch p {
	case core.CommitPhaseActive:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseActive_()
	case core.CommitPhaseDataWritten:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataWritten_()
	case core.CommitPhaseDataSynced:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataSynced_()
	case core.CommitPhaseMetaWritten:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseMetaWritten_()
	default:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseIdle_()
	}
}

func (dafnyProtocol) SelectRecoveredMeta(meta0 core.Meta, ok0 bool, meta1 core.Meta, ok1 bool) (core.Meta, error) {
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
		return core.Meta{}, fromDafnyErrCode(res.Dtor_error().(GroundKVTypes.ErrCode))
	}
	return fromDafnyMeta(res.Dtor_value().(GroundKVTypes.Meta)), nil
}

func (dafnyProtocol) ValidNextPhase(current, next core.CommitPhase) bool {
	return GroundKVEngine.Companion_Default___.ValidNextPhase(toDafnyCommitPhase(current), toDafnyCommitPhase(next))
}
