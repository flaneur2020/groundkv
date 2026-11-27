package verified

import (
	"github.com/flaneur2020/groundkv/engine"
	"github.com/flaneur2020/groundkv/engine/verified/internal/dafnygen/GroundKVEngine"
)

type dafnyCommitCore struct{}

var _ engine.CommitCore = dafnyCommitCore{}

func toDafnyCommitPhase(p engine.CommitPhase) GroundKVEngine.CommitPhase {
	switch p {
	case engine.CommitPhaseActive:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseActive_()
	case engine.CommitPhaseDataWritten:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataWritten_()
	case engine.CommitPhaseDataSynced:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseDataSynced_()
	case engine.CommitPhaseMetaWritten:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseMetaWritten_()
	default:
		return GroundKVEngine.Companion_CommitPhase_.Create_PhaseIdle_()
	}
}

func (dafnyCommitCore) ValidNextPhase(current, next engine.CommitPhase) bool {
	return GroundKVEngine.Companion_Default___.ValidNextPhase(toDafnyCommitPhase(current), toDafnyCommitPhase(next))
}
