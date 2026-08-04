// Package policy contains controller-side authorization and operational
// state. It deliberately stores metadata only: terminal frames and input
// bytes do not belong in this package.
package policy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Permission string

const (
	PermissionObserve Permission = "observe"
	PermissionControl Permission = "control"
)

func (p Permission) valid() bool { return p == PermissionObserve || p == PermissionControl }

type Grant struct {
	ID          string       `json:"id"`
	Subject     string       `json:"subject"`
	DeviceID    string       `json:"device_id,omitempty"`
	Session     string       `json:"session"`
	Permissions []Permission `json:"permissions"`
	CreatedAt   time.Time    `json:"created_at"`
	ExpiresAt   *time.Time   `json:"expires_at,omitempty"`
	RevokedAt   *time.Time   `json:"revoked_at,omitempty"`
}

type Capability struct {
	ID          string       `json:"id"`
	Token       string       `json:"token"`
	Subject     string       `json:"subject"`
	DeviceID    string       `json:"device_id"`
	Session     string       `json:"session"`
	Permissions []Permission `json:"permissions"`
	IssuedAt    time.Time    `json:"issued_at"`
	ExpiresAt   time.Time    `json:"expires_at"`
	RevokedAt   *time.Time   `json:"revoked_at,omitempty"`
}

type Lease struct {
	Session    string    `json:"session"`
	Subject    string    `json:"subject"`
	DeviceID   string    `json:"device_id,omitempty"`
	Capability string    `json:"capability"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Revocation struct {
	Version  uint64    `json:"version"`
	ID       string    `json:"id"`
	DeviceID string    `json:"device_id,omitempty"`
	Subject  string    `json:"subject,omitempty"`
	GrantID  string    `json:"grant_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// AuditEvent contains authorization metadata only. Payloads, terminal text,
// and input bytes must never be added here.
type AuditEvent struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Actor      string    `json:"actor,omitempty"`
	DeviceID   string    `json:"device_id,omitempty"`
	Session    string    `json:"session,omitempty"`
	Action     string    `json:"action"`
	RequestID  uint64    `json:"request_id,omitempty"`
	GrantID    string    `json:"grant_id,omitempty"`
	Capability string    `json:"capability,omitempty"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason,omitempty"`
}

type diskState struct {
	Version        uint32                `json:"version"`
	NextRevocation uint64                `json:"next_revocation"`
	Grants         map[string]Grant      `json:"grants"`
	Capabilities   map[string]Capability `json:"capabilities"`
	Leases         map[string]Lease      `json:"leases"`
	Revocations    []Revocation          `json:"revocations"`
	Audit          []AuditEvent          `json:"audit"`
}

const schemaVersion = 1

type Store struct {
	mu    sync.RWMutex
	path  string
	state diskState
}

func NewMemory() *Store { return &Store{state: newState()} }

// NewInMemory is an explicit alias for callers that prefer constructor names
// that describe persistence behavior.
func NewInMemory() *Store { return NewMemory() }

func New(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("policy state path is empty")
	}
	s := &Store{path: path, state: newState()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewStore is the durable constructor alias used by controller wiring.
func NewStore(path string) (*Store, error) { return New(path) }

func newState() diskState {
	return diskState{Version: schemaVersion, Grants: map[string]Grant{}, Capabilities: map[string]Capability{}, Leases: map[string]Lease{}}
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b), nil
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".policy-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		err = json.NewEncoder(tmp).Encode(s.state)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st diskState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.Version != schemaVersion {
		return fmt.Errorf("unsupported policy schema version %d", st.Version)
	}
	if st.Grants == nil {
		st.Grants = map[string]Grant{}
	}
	if st.Capabilities == nil {
		st.Capabilities = map[string]Capability{}
	}
	if st.Leases == nil {
		st.Leases = map[string]Lease{}
	}
	s.state = st
	return nil
}

func contains(ps []Permission, want Permission) bool {
	for _, p := range ps {
		if p == want {
			return true
		}
	}
	return false
}
func validPermissions(ps []Permission) bool {
	if len(ps) == 0 {
		return false
	}
	for _, p := range ps {
		if !p.valid() {
			return false
		}
	}
	return true
}

func (s *Store) AddGrant(subject, deviceID, session string, permissions []Permission, expiresAt *time.Time) (Grant, error) {
	if subject == "" || session == "" || !validPermissions(permissions) {
		return Grant{}, errors.New("subject, session, and valid permissions are required")
	}
	id, err := randomID("grant")
	if err != nil {
		return Grant{}, err
	}
	g := Grant{ID: id, Subject: subject, DeviceID: deviceID, Session: session, Permissions: append([]Permission(nil), permissions...), CreatedAt: time.Now().UTC(), ExpiresAt: expiresAt}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Grants[id] = g
	if err := s.persistLocked(); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// GrantACL is a descriptive alias for AddGrant used by controller handlers.
func (s *Store) GrantACL(subject, deviceID, session string, permissions []Permission, expiresAt *time.Time) (Grant, error) {
	return s.AddGrant(subject, deviceID, session, permissions, expiresAt)
}

func (s *Store) Grant(id string) (Grant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.state.Grants[id]
	g.Permissions = append([]Permission(nil), g.Permissions...)
	return g, ok
}

func (s *Store) CanObserve(subject, deviceID, session string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authorizeLocked(subject, deviceID, session, PermissionObserve, now) == nil
}

func (s *Store) CanControl(subject, deviceID, session string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authorizeLocked(subject, deviceID, session, PermissionControl, now) == nil
}

func (s *Store) RevokeGrant(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.state.Grants[id]
	if !ok {
		return errors.New("grant not found")
	}
	now := time.Now().UTC()
	g.RevokedAt = &now
	s.state.Grants[id] = g
	if err := s.appendRevocationLocked(Revocation{GrantID: id, Reason: reason, At: now}); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) IssueCapability(subject, deviceID, session string, permission Permission, ttl time.Duration) (Capability, error) {
	if ttl <= 0 {
		return Capability{}, errors.New("capability ttl must be positive")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorizeLocked(subject, deviceID, session, permission, now); err != nil {
		return Capability{}, err
	}
	id, err := randomID("cap")
	if err != nil {
		return Capability{}, err
	}
	tok, err := randomID("tok")
	if err != nil {
		return Capability{}, err
	}
	c := Capability{ID: id, Token: tok, Subject: subject, DeviceID: deviceID, Session: session, Permissions: []Permission{permission}, IssuedAt: now, ExpiresAt: now.Add(ttl)}
	s.state.Capabilities[id] = c
	if err := s.persistLocked(); err != nil {
		return Capability{}, err
	}
	return c, nil
}

func (s *Store) ValidateCapability(c Capability, permission Permission, now time.Time) error {
	_, err := s.AuthenticateCapability(c.ID, c.Token, permission, now)
	return err
}

// AuthenticateCapability validates an opaque capability ID/token pair and
// returns the controller's canonical record. Callers must use the returned
// record for routing and authorization rather than trusting fields supplied by
// a client alongside the token.
func (s *Store) AuthenticateCapability(id, token string, permission Permission, now time.Time) (Capability, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.state.Capabilities[id]
	if !ok || stored.Token != token {
		return Capability{}, errors.New("capability is unknown")
	}
	for _, r := range s.state.Revocations {
		if r.GrantID == "" && ((r.DeviceID != "" && r.DeviceID == stored.DeviceID) || (r.Subject != "" && r.Subject == stored.Subject)) {
			return Capability{}, errors.New("capability has been revoked")
		}
	}
	if err := s.authorizeLocked(stored.Subject, stored.DeviceID, stored.Session, permission, now); err != nil {
		return Capability{}, errors.New("capability grant is no longer active")
	}
	if err := validateCapabilityLocked(stored, permission, now); err != nil {
		return Capability{}, err
	}
	stored.Permissions = append([]Permission(nil), stored.Permissions...)
	return stored, nil
}

func validateCapabilityLocked(c Capability, permission Permission, now time.Time) error {
	if !permission.valid() || !contains(c.Permissions, permission) {
		return errors.New("capability lacks permission")
	}
	if c.RevokedAt != nil || !now.Before(c.ExpiresAt) {
		return errors.New("capability is expired or revoked")
	}
	return nil
}

func (s *Store) authorizeLocked(subject, deviceID, session string, permission Permission, now time.Time) error {
	for _, r := range s.state.Revocations {
		if r.GrantID == "" && ((r.DeviceID != "" && r.DeviceID == deviceID) || (r.Subject != "" && r.Subject == subject)) {
			return errors.New("access denied: identity is revoked")
		}
	}
	for _, g := range s.state.Grants {
		if g.Subject != subject || (g.Session != session && g.Session != "*") || (g.DeviceID != "" && g.DeviceID != deviceID) || g.RevokedAt != nil || (g.ExpiresAt != nil && !now.Before(*g.ExpiresAt)) {
			continue
		}
		if contains(g.Permissions, permission) {
			return nil
		}
	}
	return errors.New("access denied")
}

func (s *Store) RevokeDevice(deviceID, reason string) error {
	if deviceID == "" {
		return errors.New("device ID is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if err := s.appendRevocationLocked(Revocation{DeviceID: deviceID, Reason: reason, At: now}); err != nil {
		return err
	}
	for id, c := range s.state.Capabilities {
		if c.DeviceID == deviceID {
			c.RevokedAt = &now
			s.state.Capabilities[id] = c
		}
	}
	return s.persistLocked()
}

// RevokeSubject invalidates all capabilities issued to an account identity.
func (s *Store) RevokeSubject(subject, reason string) error {
	if subject == "" {
		return errors.New("subject is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if err := s.appendRevocationLocked(Revocation{Subject: subject, Reason: reason, At: now}); err != nil {
		return err
	}
	for id, c := range s.state.Capabilities {
		if c.Subject == subject {
			c.RevokedAt = &now
			s.state.Capabilities[id] = c
		}
	}
	return s.persistLocked()
}

func (s *Store) IsDeviceRevoked(deviceID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Revocations {
		if r.DeviceID == deviceID {
			return true
		}
	}
	return false
}

func (s *Store) IsSubjectRevoked(subject string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Revocations {
		if r.Subject == subject {
			return true
		}
	}
	return false
}

func (s *Store) RevocationVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.NextRevocation
}

func (s *Store) appendRevocationLocked(r Revocation) error {
	s.state.NextRevocation++
	r.Version = s.state.NextRevocation
	if r.ID == "" {
		id, err := randomID("rev")
		if err != nil {
			return err
		}
		r.ID = id
	}
	s.state.Revocations = append(s.state.Revocations, r)
	return nil
}
func (s *Store) RevocationsSince(version uint64) []Revocation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Revocation{}
	for _, r := range s.state.Revocations {
		if r.Version > version {
			out = append(out, r)
		}
	}
	return out
}

// ApplyRevocations records externally observed revocations. Replays are
// ignored by ID, making propagation safe across reconnects.
func (s *Store) ApplyRevocations(revs []Revocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(s.state.Revocations))
	for _, r := range s.state.Revocations {
		if r.ID != "" {
			seen[r.ID] = struct{}{}
		}
	}
	for _, r := range revs {
		if r.ID != "" {
			if _, ok := seen[r.ID]; ok {
				continue
			}
		}
		if r.At.IsZero() {
			r.At = time.Now().UTC()
		}
		if err := s.appendRevocationLocked(r); err != nil {
			return err
		}
		if r.GrantID == "" && (r.DeviceID != "" || r.Subject != "") {
			for id, c := range s.state.Capabilities {
				if (r.DeviceID != "" && c.DeviceID == r.DeviceID) || (r.Subject != "" && c.Subject == r.Subject) {
					c.RevokedAt = &r.At
					s.state.Capabilities[id] = c
				}
			}
		}
	}
	return s.persistLocked()
}

func (s *Store) AcquireLease(capability Capability, ttl time.Duration, now time.Time) (Lease, error) {
	if ttl <= 0 {
		return Lease{}, errors.New("lease ttl must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.state.Capabilities[capability.ID]
	if !ok || stored.Token != capability.Token {
		return Lease{}, errors.New("capability is unknown")
	}
	if err := validateCapabilityLocked(stored, PermissionControl, now); err != nil {
		return Lease{}, err
	}
	if err := s.authorizeLocked(stored.Subject, stored.DeviceID, stored.Session, PermissionControl, now); err != nil {
		return Lease{}, errors.New("capability grant is no longer active")
	}
	for _, r := range s.state.Revocations {
		if r.GrantID == "" && r.DeviceID == capability.DeviceID {
			return Lease{}, errors.New("device is revoked")
		}
	}
	if old, ok := s.state.Leases[capability.Session]; ok && now.Before(old.ExpiresAt) && old.Capability != capability.ID {
		return Lease{}, errors.New("control lease is held")
	}
	l := Lease{Session: capability.Session, Subject: capability.Subject, DeviceID: capability.DeviceID, Capability: capability.ID, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
	s.state.Leases[l.Session] = l
	if err := s.persistLocked(); err != nil {
		return Lease{}, err
	}
	return l, nil
}

func (s *Store) RenewLease(session, subject, capabilityID string, ttl time.Duration, now time.Time) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.state.Leases[session]
	if !ok || l.Subject != subject || l.Capability != capabilityID || !now.Before(l.ExpiresAt) {
		return Lease{}, errors.New("control lease is not held")
	}
	if ttl <= 0 {
		return Lease{}, errors.New("lease ttl must be positive")
	}
	l.ExpiresAt = now.Add(ttl)
	s.state.Leases[session] = l
	return l, s.persistLocked()
}
func (s *Store) ReleaseLease(session, subject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.state.Leases[session]
	if !ok {
		return nil
	}
	if l.Subject != subject {
		return errors.New("control lease is owned by another subject")
	}
	delete(s.state.Leases, session)
	return s.persistLocked()
}
func (s *Store) ReapLeases(now time.Time) []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []Lease
	for k, l := range s.state.Leases {
		if !now.Before(l.ExpiresAt) {
			expired = append(expired, l)
			delete(s.state.Leases, k)
		}
	}
	if len(expired) > 0 {
		_ = s.persistLocked()
	}
	return expired
}
func (s *Store) Lease(session string) (Lease, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.state.Leases[session]
	return l, ok
}

func (s *Store) Audit(e AuditEvent) error {
	if e.Action == "" || e.Outcome == "" {
		return errors.New("audit action and outcome are required")
	}
	e.ID, _ = randomID("audit")
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Audit = append(s.state.Audit, e)
	return s.persistLocked()
}
func (s *Store) AuditEvents(limit int) []AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.state.Audit) {
		limit = len(s.state.Audit)
	}
	out := append([]AuditEvent(nil), s.state.Audit[len(s.state.Audit)-limit:]...)
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
