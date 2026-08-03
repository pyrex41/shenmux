package client

import (
	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/screen"
)

type Kind = protocol.Kind
type Meta = protocol.Meta
type Message = protocol.Message
type Archive = protocol.Archive
type Checkpoint = protocol.Checkpoint
type Event = protocol.Event
type EventKind = protocol.EventKind
type Delta = screen.Delta
type State = screen.State
type Renderer = screen.Renderer

const (
	KindDelta   = protocol.KindDelta
	KindControl = protocol.KindControl
	KindExit    = protocol.KindExit

	EventDelta   = protocol.EventDelta
	EventControl = protocol.EventControl
	EventExit    = protocol.EventExit
)
