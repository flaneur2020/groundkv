// Package gocore provides GroundKV's default pure-Go stdlib implementation of
// engine/core.Core. It imports only engine/core and the Go standard library,
// guaranteeing a strictly unidirectional boundary from the engine I/O shell.
package gocore

import "github.com/flaneur2020/groundkv/engine/core"

type goCore struct {
	goCodec
	goBTreeNode
	goFreelist
	goProtocol
}

var (
	_ core.Core = goCore{}

	defaultCore core.Core = goCore{}
)

// New returns GroundKV's default pure-Go core.Core implementation.
func New() core.Core { return defaultCore }
