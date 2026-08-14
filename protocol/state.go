package protocol

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"

	"github.com/pyrex41/shenmux/screen"
)

const (
	stateFormatVersion   = uint16(2)
	maxStateUncompressed = 64 << 20
	MaxDeltaPayloadSize  = 16 << 20
	DefaultHistoryRows   = 2_000
	DefaultMaxTailEvents = 2_048
	DefaultMaxTailBytes  = 8 << 20
	maxControlOwnerBytes = 128
)

type EventKind string

const (
	EventDelta   EventKind = "screen-delta"
	EventControl EventKind = "control-owner"
	EventExit    EventKind = "exit"
)

type Checkpoint struct {
	Seq          uint64       `json:"seq"`
	Screen       screen.State `json:"screen"`
	ControlOwner string       `json:"control_owner,omitempty"`
	Exited       bool         `json:"exited,omitempty"`
	ExitCode     int          `json:"exit_code,omitempty"`
}

func (c Checkpoint) Clone() Checkpoint {
	c.Screen = c.Screen.Clone()
	return c
}

func (c Checkpoint) Validate(historyLimit int) error {
	if historyLimit < 0 || historyLimit > screen.MaxHistoryRows {
		return fmt.Errorf("history limit %d outside 0..%d", historyLimit, screen.MaxHistoryRows)
	}
	if err := c.Screen.Validate(); err != nil {
		return fmt.Errorf("checkpoint screen: %w", err)
	}
	if len(c.Screen.History) > historyLimit {
		return fmt.Errorf("checkpoint history has %d rows, configured limit %d", len(c.Screen.History), historyLimit)
	}
	if err := validateOwner(c.ControlOwner); err != nil {
		return err
	}
	return nil
}

type Event struct {
	Kind         EventKind     `json:"kind"`
	Seq          uint64        `json:"seq"`
	Delta        *screen.Delta `json:"delta,omitempty"`
	ControlOwner string        `json:"control_owner,omitempty"`
	ExitCode     int           `json:"exit_code,omitempty"`
}

func (e Event) Clone() Event {
	if e.Delta != nil {
		delta := e.Delta.Clone()
		e.Delta = &delta
	}
	return e
}

func (e Event) Validate() error {
	if e.Seq == 0 {
		return errors.New("state event sequence must be positive")
	}
	switch e.Kind {
	case EventDelta:
		if e.Delta == nil {
			return errors.New("screen-delta event has no delta")
		}
		if err := e.Delta.Validate(); err != nil {
			return fmt.Errorf("screen-delta event: %w", err)
		}
		if e.ControlOwner != "" {
			return errors.New("screen-delta event carries a control owner")
		}
	case EventControl:
		if e.Delta != nil {
			return errors.New("control event carries a screen delta")
		}
		if err := validateOwner(e.ControlOwner); err != nil {
			return err
		}
	case EventExit:
		if e.Delta != nil || e.ControlOwner != "" {
			return errors.New("exit event carries unrelated state")
		}
	default:
		return fmt.Errorf("unknown state event kind %q", e.Kind)
	}
	return nil
}

type Archive struct {
	Format       uint16     `json:"format"`
	HistoryLimit int        `json:"history_limit"`
	Checkpoint   Checkpoint `json:"checkpoint"`
	Tail         []Event    `json:"tail,omitempty"`
}

func (a Archive) Clone() Archive {
	out := Archive{
		Format: a.Format, HistoryLimit: a.HistoryLimit,
		Checkpoint: a.Checkpoint.Clone(), Tail: make([]Event, len(a.Tail)),
	}
	for i, event := range a.Tail {
		out.Tail[i] = event.Clone()
	}
	return out
}

func (a Archive) LastSeq() uint64 {
	if len(a.Tail) == 0 {
		return a.Checkpoint.Seq
	}
	return a.Tail[len(a.Tail)-1].Seq
}

func (a Archive) Validate() error {
	if a.Format != stateFormatVersion {
		return fmt.Errorf("unsupported state archive format %d", a.Format)
	}
	if err := a.Checkpoint.Validate(a.HistoryLimit); err != nil {
		return err
	}
	last := a.Checkpoint.Seq
	for i, event := range a.Tail {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("tail event %d: %w", i, err)
		}
		if event.Seq != last+1 {
			return fmt.Errorf("tail event %d has seq %d, expected %d", i, event.Seq, last+1)
		}
		last = event.Seq
	}
	_, err := a.Current()
	return err
}

// Current reconstructs current state exclusively from interpreted checkpoint
// and deltas. No terminal control stream is parsed or replayed.
func (a Archive) Current() (Checkpoint, error) {
	if a.Format != stateFormatVersion {
		return Checkpoint{}, fmt.Errorf("unsupported state archive format %d", a.Format)
	}
	if err := a.Checkpoint.Validate(a.HistoryLimit); err != nil {
		return Checkpoint{}, err
	}
	current := a.Checkpoint.Clone()
	last := current.Seq
	for i, event := range a.Tail {
		if err := event.Validate(); err != nil {
			return Checkpoint{}, fmt.Errorf("tail event %d: %w", i, err)
		}
		if event.Seq != last+1 {
			return Checkpoint{}, fmt.Errorf("tail event %d has seq %d, expected %d", i, event.Seq, last+1)
		}
		switch event.Kind {
		case EventDelta:
			state, err := screen.Apply(current.Screen, *event.Delta, a.HistoryLimit)
			if err != nil {
				return Checkpoint{}, fmt.Errorf("apply tail event %d: %w", i, err)
			}
			current.Screen = state
		case EventControl:
			current.ControlOwner = event.ControlOwner
		case EventExit:
			current.Exited = true
			current.ExitCode = event.ExitCode
		}
		current.Seq = event.Seq
		last = event.Seq
	}
	if err := current.Validate(a.HistoryLimit); err != nil {
		return Checkpoint{}, err
	}
	return current, nil
}

func EncodeArchive(archive Archive) ([]byte, error) {
	if err := archive.Validate(); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(archive)
	if err != nil {
		return nil, fmt.Errorf("encode state archive: %w", err)
	}
	if len(plain) > maxStateUncompressed {
		return nil, fmt.Errorf("state archive exceeds %d uncompressed bytes", maxStateUncompressed)
	}
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err = zw.Write(plain); err == nil {
		err = zw.Close()
	} else {
		_ = zw.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("compress state archive: %w", err)
	}
	if out.Len() > MaxPayloadSize {
		return nil, fmt.Errorf("compressed state archive exceeds %d bytes", MaxPayloadSize)
	}
	return out.Bytes(), nil
}

func DecodeArchive(payload []byte) (Archive, error) {
	if len(payload) == 0 {
		return Archive{}, errors.New("empty state archive")
	}
	if len(payload) > MaxPayloadSize {
		return Archive{}, fmt.Errorf("compressed state archive exceeds %d bytes", MaxPayloadSize)
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return Archive{}, fmt.Errorf("open state archive gzip: %w", err)
	}
	plain, readErr := io.ReadAll(io.LimitReader(zr, maxStateUncompressed+1))
	closeErr := zr.Close()
	if readErr != nil {
		return Archive{}, fmt.Errorf("decompress state archive: %w", readErr)
	}
	if closeErr != nil {
		return Archive{}, fmt.Errorf("close state archive gzip: %w", closeErr)
	}
	if len(plain) > maxStateUncompressed {
		return Archive{}, fmt.Errorf("state archive exceeds %d uncompressed bytes", maxStateUncompressed)
	}
	var archive Archive
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&archive); err != nil {
		return Archive{}, fmt.Errorf("decode state archive: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Archive{}, err
	}
	if err := archive.Validate(); err != nil {
		return Archive{}, err
	}
	return archive, nil
}

func EncodeDelta(delta screen.Delta) ([]byte, error) {
	if err := delta.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(delta)
	if err != nil {
		return nil, fmt.Errorf("encode screen delta: %w", err)
	}
	if len(payload) > MaxDeltaPayloadSize {
		return nil, fmt.Errorf("screen delta exceeds %d bytes", MaxDeltaPayloadSize)
	}
	return payload, nil
}

func DecodeDelta(payload []byte) (screen.Delta, error) {
	if len(payload) == 0 {
		return screen.Delta{}, errors.New("empty screen delta")
	}
	if len(payload) > MaxDeltaPayloadSize {
		return screen.Delta{}, fmt.Errorf("screen delta exceeds %d bytes", MaxDeltaPayloadSize)
	}
	var delta screen.Delta
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&delta); err != nil {
		return screen.Delta{}, fmt.Errorf("decode screen delta: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return screen.Delta{}, err
	}
	if err := delta.Validate(); err != nil {
		return screen.Delta{}, err
	}
	return delta, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON payload contains trailing value")
		}
		return fmt.Errorf("check JSON trailing data: %w", err)
	}
	return nil
}

func validateOwner(owner string) error {
	if owner == "" {
		return nil
	}
	if len(owner) > maxControlOwnerBytes {
		return fmt.Errorf("control owner exceeds %d bytes", maxControlOwnerBytes)
	}
	if !utf8.ValidString(owner) {
		return errors.New("control owner is not UTF-8")
	}
	for _, r := range owner {
		if r == 0 {
			return errors.New("control owner contains NUL")
		}
	}
	return nil
}

type StoreLimits struct {
	HistoryRows   int
	MaxTailEvents int
	MaxTailBytes  int
}

func DefaultStoreLimits() StoreLimits {
	return StoreLimits{
		HistoryRows:   DefaultHistoryRows,
		MaxTailEvents: DefaultMaxTailEvents,
		MaxTailBytes:  DefaultMaxTailBytes,
	}
}

func (l StoreLimits) validate() error {
	if l.HistoryRows < 0 || l.HistoryRows > screen.MaxHistoryRows {
		return fmt.Errorf("history rows %d outside 0..%d", l.HistoryRows, screen.MaxHistoryRows)
	}
	if l.MaxTailEvents <= 0 || l.MaxTailEvents > 1_000_000 {
		return fmt.Errorf("max tail events %d outside 1..1000000", l.MaxTailEvents)
	}
	if l.MaxTailBytes <= 0 || l.MaxTailBytes > maxStateUncompressed {
		return fmt.Errorf("max tail bytes %d outside 1..%d", l.MaxTailBytes, maxStateUncompressed)
	}
	return nil
}

type StoreStats struct {
	CheckpointSeq uint64
	LastSeq       uint64
	TailEvents    int
	TailBytes     int
	Compactions   uint64
}

// Store retains one canonical checkpoint and a bounded transition tail. When
// either tail budget is crossed, the caller-supplied current state atomically
// becomes the next checkpoint and all older transitions are discarded.
type Store struct {
	mu          sync.RWMutex
	limits      StoreLimits
	checkpoint  Checkpoint
	tail        []Event
	tailBytes   int
	compactions uint64
}

func NewStore(initial Checkpoint, limits StoreLimits) (*Store, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if err := initial.Validate(limits.HistoryRows); err != nil {
		return nil, err
	}
	return &Store{limits: limits, checkpoint: initial.Clone()}, nil
}

func (s *Store) Append(event Event, current Checkpoint) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if err := current.Validate(s.limits.HistoryRows); err != nil {
		return fmt.Errorf("current checkpoint: %w", err)
	}
	if current.Seq != event.Seq {
		return fmt.Errorf("current checkpoint seq %d does not match event seq %d", current.Seq, event.Seq)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.checkpoint.Seq
	if len(s.tail) != 0 {
		last = s.tail[len(s.tail)-1].Seq
	}
	if event.Seq != last+1 {
		return fmt.Errorf("state event seq %d, expected %d", event.Seq, last+1)
	}

	candidateTail := append(cloneEvents(s.tail), event.Clone())
	candidate := Archive{
		Format: stateFormatVersion, HistoryLimit: s.limits.HistoryRows,
		Checkpoint: s.checkpoint.Clone(), Tail: candidateTail,
	}
	derived, err := candidate.Current()
	if err != nil {
		return err
	}
	if !checkpointEqual(derived, current) {
		return errors.New("caller current state does not equal checkpoint plus appended event")
	}

	s.tail = candidateTail
	s.tailBytes += len(encoded)
	if len(s.tail) > s.limits.MaxTailEvents || s.tailBytes > s.limits.MaxTailBytes {
		s.checkpoint = current.Clone()
		s.tail = nil
		s.tailBytes = 0
		s.compactions++
	}
	return nil
}

func (s *Store) Snapshot() Archive {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Archive{
		Format: stateFormatVersion, HistoryLimit: s.limits.HistoryRows,
		Checkpoint: s.checkpoint.Clone(), Tail: cloneEvents(s.tail),
	}
}

func (s *Store) Stats() StoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	last := s.checkpoint.Seq
	if len(s.tail) != 0 {
		last = s.tail[len(s.tail)-1].Seq
	}
	return StoreStats{
		CheckpointSeq: s.checkpoint.Seq, LastSeq: last,
		TailEvents: len(s.tail), TailBytes: s.tailBytes, Compactions: s.compactions,
	}
}

func (s *Store) HistoryLimit() int { return s.limits.HistoryRows }

func cloneEvents(events []Event) []Event {
	out := make([]Event, len(events))
	for i, event := range events {
		out[i] = event.Clone()
	}
	return out
}

func checkpointEqual(a, b Checkpoint) bool {
	return a.Seq == b.Seq && a.ControlOwner == b.ControlOwner &&
		a.Exited == b.Exited && a.ExitCode == b.ExitCode &&
		screen.EqualState(a.Screen, b.Screen)
}
