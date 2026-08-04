package shenguard

// This file is the Go-facing boundary for the Shen control-plane reducer.
// Shen owns command authorization and state transitions; Go owns the opaque
// payload store and executes the declarative effects returned here.

import (
	"fmt"

	"github.com/pyrex41/shenmux/internal/shenmodel"
)

// State is kept as an alias for compatibility with the existing guard API.
// Callers should treat it as opaque and only use the accessors on Session.
type State = Session

type CommandKind string

const (
	CommandAttach         CommandKind = "attach"
	CommandDetach         CommandKind = "detach"
	CommandAcquireControl CommandKind = "acquire-control"
	CommandReleaseControl CommandKind = "release-control"
	CommandInput          CommandKind = "input"
	CommandResize         CommandKind = "resize"
	CommandBeginAttach    CommandKind = "begin-attach"
	CommandFinishAttach   CommandKind = "finish-attach"
	CommandBeginSnapshot  CommandKind = "begin-snapshot"
	CommandFinishSnapshot CommandKind = "finish-snapshot"
	CommandPTYOutput      CommandKind = "pty-output"
	CommandProcessExit    CommandKind = "process-exit"
	CommandLeaseExpired   CommandKind = "lease-expired"
	CommandResync         CommandKind = "resync"
)

// TokenID identifies bytes held by a Go-side opaque payload store. Shen sees
// only this bounded scalar and never copies arbitrary PTY or wire payloads.
type TokenID uint64

// Command is a typed control-plane input. Fields not used by Kind are ignored.
type Command struct {
	Kind     CommandKind
	Client   ClientID
	Token    TokenID
	Dim      Dimensions
	Snapshot Snapshot
	Code     int
}

type Reason string

const (
	ReasonAlreadyAttached       Reason = "already-attached"
	ReasonNotAttached           Reason = "not-attached"
	ReasonWriterLocked          Reason = "writer-locked"
	ReasonWriterUnlocked        Reason = "writer-unlocked"
	ReasonSequence              Reason = "sequence"
	ReasonExited                Reason = "exited"
	ReasonNoControl             Reason = "no-control"
	ReasonControlOwned          Reason = "control-owned"
	ReasonSnapshotMismatch      Reason = "snapshot-mismatch"
	ReasonSnapshotOwnerMismatch Reason = "snapshot-owner-mismatch"
	ReasonInvalidDimensions     Reason = "invalid-dimensions"
	ReasonUnknownCommand        Reason = "unknown-command"
)

type EffectKind string

const (
	EffectWritePTY        EffectKind = "write-pty"
	EffectResizePTY       EffectKind = "resize-pty"
	EffectPublish         EffectKind = "publish"
	EffectCaptureSnapshot EffectKind = "capture-snapshot"
	EffectReply           EffectKind = "reply"
)

// Effect is declarative; runtime adapters execute it. Payloads are referenced
// by TokenID and remain in Go-owned storage.
type Effect struct {
	Kind       EffectKind
	Client     ClientID
	Token      TokenID
	Seq        SeqNo
	Dimensions Dimensions
	EventKind  string
	Code       int
}

type Result struct {
	Accepted bool
	State    State
	Effects  []Effect
	Reason   Reason
}

func (r Result) IsRejected() bool { return !r.Accepted }

// Reduce submits one command to the executable Shen model.
func Reduce(state State, command Command) (Result, error) {
	value, err := callSemantic("mux.reduce", sessionValue(state), commandValue(command))
	if err != nil {
		return Result{}, err
	}
	return resultFromValue(value)
}

func commandValue(c Command) shenmodel.List {
	base := shenmodel.List{string(c.Kind)}
	switch c.Kind {
	case CommandAttach, CommandDetach, CommandAcquireControl, CommandReleaseControl,
		CommandLeaseExpired, CommandResync, CommandBeginAttach:
		return append(base, c.Client.String())
	case CommandInput, CommandPTYOutput:
		if c.Kind == CommandInput {
			return append(base, c.Client.String(), uint64(c.Token))
		}
		return append(base, uint64(c.Token))
	case CommandResize:
		return append(base, c.Client.String(), dimensionValue(c.Dim))
	case CommandFinishAttach:
		return append(base, c.Client.String(), snapshotValue(c.Snapshot))
	case CommandFinishSnapshot:
		return append(base, snapshotValue(c.Snapshot))
	case CommandProcessExit:
		return append(base, c.Code)
	default:
		return base
	}
}

func resultFromValue(value any) (Result, error) {
	list, ok := value.(shenmodel.List)
	if !ok || len(list) < 2 {
		return Result{}, fmt.Errorf("Shen reducer returned invalid result %T", value)
	}
	tag, ok := list[0].(string)
	if !ok {
		return Result{}, fmt.Errorf("Shen reducer result tag has type %T", list[0])
	}
	switch tag {
	case "accepted":
		state, err := sessionFromValue(list[1])
		if err != nil {
			return Result{}, err
		}
		effects := shenmodel.List{}
		if len(list) > 2 {
			var ok bool
			effects, ok = list[2].(shenmodel.List)
			if !ok {
				return Result{}, fmt.Errorf("Shen reducer effects have type %T", list[2])
			}
		}
		parsed, err := effectsFromValue(effects)
		if err != nil {
			return Result{}, err
		}
		return Result{Accepted: true, State: state, Effects: parsed}, nil
	case "rejected":
		reason := Reason("rejected")
		if len(list) > 1 {
			if text, ok := list[1].(string); ok {
				reason = Reason(text)
			}
		}
		return Result{Reason: reason}, nil
	default:
		return Result{}, fmt.Errorf("unknown Shen reducer result tag %q", tag)
	}
}

func effectsFromValue(values shenmodel.List) ([]Effect, error) {
	out := make([]Effect, 0, len(values))
	for i, raw := range values {
		form, ok := raw.(shenmodel.List)
		if !ok || len(form) == 0 {
			return nil, fmt.Errorf("Shen effect %d has invalid shape", i)
		}
		tag, ok := form[0].(string)
		if !ok {
			return nil, fmt.Errorf("Shen effect %d tag has type %T", i, form[0])
		}
		e := Effect{Kind: EffectKind(tag)}
		switch e.Kind {
		case EffectWritePTY:
			if len(form) != 3 {
				return nil, fmt.Errorf("write-pty effect has %d fields", len(form))
			}
			cid, err := NewClientIDString(form[1])
			if err != nil {
				return nil, err
			}
			e.Client = cid
			tok, ok := valueAsUint64(form[2])
			if !ok {
				return nil, fmt.Errorf("write-pty token has type %T", form[2])
			}
			e.Token = TokenID(tok)
		case EffectResizePTY:
			if len(form) != 2 {
				return nil, fmt.Errorf("resize-pty effect has %d fields", len(form))
			}
			dim, err := dimensionFromValue(form[1])
			if err != nil {
				return nil, err
			}
			e.Dimensions = dim
		case EffectPublish:
			if len(form) != 4 {
				return nil, fmt.Errorf("publish effect has %d fields", len(form))
			}
			seq, ok := valueAsUint64(form[1])
			if !ok {
				return nil, fmt.Errorf("publish sequence has type %T", form[1])
			}
			e.Seq = NewSeqNo(seq)
			kind, ok := form[2].(string)
			if !ok {
				return nil, fmt.Errorf("publish kind has type %T", form[2])
			}
			e.EventKind = kind
			if kind == "exit" {
				code, ok := valueAsInt(form[3])
				if !ok {
					return nil, fmt.Errorf("publish exit code has type %T", form[3])
				}
				e.Code = code
			} else {
				tok, ok := valueAsUint64(form[3])
				if !ok {
					return nil, fmt.Errorf("publish token has type %T", form[3])
				}
				e.Token = TokenID(tok)
			}
		case EffectCaptureSnapshot:
			if len(form) != 1 {
				return nil, fmt.Errorf("capture-snapshot effect has %d fields", len(form))
			}
		case EffectReply:
			if len(form) != 3 {
				return nil, fmt.Errorf("reply effect has %d fields", len(form))
			}
			cid, err := NewClientIDString(form[1])
			if err != nil {
				return nil, err
			}
			e.Client = cid
			tok, ok := valueAsUint64(form[2])
			if !ok {
				return nil, fmt.Errorf("reply token has type %T", form[2])
			}
			e.Token = TokenID(tok)
		default:
			return nil, fmt.Errorf("unknown Shen effect %q", tag)
		}
		out = append(out, e)
	}
	return out, nil
}

func NewClientIDString(value any) (ClientID, error) {
	text, ok := value.(string)
	if !ok {
		return ClientID{}, fmt.Errorf("client id has type %T", value)
	}
	return NewClientID(text)
}

func valueAsInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case uint64:
		converted := int(n)
		return converted, converted >= 0 && uint64(converted) == n
	default:
		return 0, false
	}
}
