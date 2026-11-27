package gocore

import "github.com/flaneur2020/groundkv/engine/core"

type goProtocol struct{}

var _ core.Protocol = goProtocol{}

func (goProtocol) SelectRecoveredMeta(meta0 core.Meta, ok0 bool, meta1 core.Meta, ok1 bool) (core.Meta, error) {
	switch {
	case ok0 && ok1:
		if meta0.TxID >= meta1.TxID {
			return meta0, nil
		}
		return meta1, nil
	case ok0:
		return meta0, nil
	case ok1:
		return meta1, nil
	default:
		return core.Meta{}, core.ErrCorruptedMeta
	}
}

func (goProtocol) ValidNextPhase(current, next core.CommitPhase) bool {
	return (current == core.CommitPhaseIdle && next == core.CommitPhaseActive) ||
		(current == core.CommitPhaseActive && next == core.CommitPhaseDataWritten) ||
		(current == core.CommitPhaseDataWritten && next == core.CommitPhaseDataSynced) ||
		(current == core.CommitPhaseDataSynced && next == core.CommitPhaseMetaWritten) ||
		(current == core.CommitPhaseMetaWritten && next == core.CommitPhaseIdle) ||
		(current != core.CommitPhaseIdle && next == core.CommitPhaseIdle)
}
