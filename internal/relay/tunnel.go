package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pyrex41/shenmux/internal/policy"
	"github.com/pyrex41/shenmux/internal/protocol"
)

type enrollmentRecord struct{ expires time.Time }
type deviceRecord struct {
	public ed25519.PublicKey
	token  []byte
}

// EnrollmentStore is a small in-memory store suitable for tests and a
// development controller. Production controllers should replace it with a
// durable implementation while retaining the atomic Consume semantics.
type EnrollmentStore struct {
	mu       sync.Mutex
	enroll   map[[32]byte]enrollmentRecord
	devices  map[string]deviceRecord
	sequence uint64
}

func NewEnrollmentStore() *EnrollmentStore {
	return &EnrollmentStore{enroll: make(map[[32]byte]enrollmentRecord), devices: make(map[string]deviceRecord)}
}

func (s *EnrollmentStore) Create(ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	code, err := NewEnrollmentCode()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.enroll == nil {
		s.enroll = make(map[[32]byte]enrollmentRecord)
	}
	if s.devices == nil {
		s.devices = make(map[string]deviceRecord)
	}
	s.enroll[HashEnrollmentCode(code)] = enrollmentRecord{expires: time.Now().Add(ttl)}
	s.mu.Unlock()
	return code, nil
}

func (s *EnrollmentStore) Consume(code string, pub ed25519.PublicKey) (DeviceCredential, error) {
	if len(pub) != ed25519.PublicKeySize {
		return DeviceCredential{}, errors.New("invalid device public key")
	}
	hash := HashEnrollmentCode(code)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enroll == nil {
		return DeviceCredential{}, errors.New("enrollment store is not initialized")
	}
	rec, ok := s.enroll[hash]
	if !ok || !now.Before(rec.expires) {
		return DeviceCredential{}, errors.New("enrollment code is invalid or expired")
	}
	delete(s.enroll, hash) // consume atomically before issuing a credential
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return DeviceCredential{}, fmt.Errorf("generate device credential: %w", err)
	}
	s.sequence++
	deviceID := fmt.Sprintf("device-%d", s.sequence)
	s.devices[deviceID] = deviceRecord{public: append(ed25519.PublicKey(nil), pub...), token: append([]byte(nil), token...)}
	return DeviceCredential{DeviceID: deviceID, Token: token, ExpiresAt: now.Add(30 * 24 * time.Hour)}, nil
}

func (s *EnrollmentStore) authenticate(id string, token []byte) (ed25519.PublicKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.devices[id]
	if !ok || len(token) != len(rec.token) || subtle.ConstantTimeCompare(token, rec.token) != 1 {
		return nil, false
	}
	return append(ed25519.PublicKey(nil), rec.public...), true
}

type helloPayload struct {
	DeviceID string `json:"device_id"`
	Token    []byte `json:"token"`
	Origin   string `json:"origin"`
	Nonce    []byte `json:"nonce"`
}

type challengePayload struct {
	Nonce []byte `json:"nonce"`
}

// Controller is a minimal authenticated relay endpoint. It implements the
// enrollment and agent handshake, leaving session multiplexing to callers.
type Controller struct {
	Store               *EnrollmentStore
	Policy              *policy.Store
	Origin              string
	Upgrader            websocket.Upgrader
	CapabilityTTL       time.Duration
	ControlLeaseTTL     time.Duration
	AuthenticateBrowser func(*http.Request) (string, error)
	mu                  sync.Mutex
	agents              map[string]*controllerAgent
	streams             map[streamKey]*authorizedStream
	usedCapabilities    map[string]struct{}
}

type streamKey struct{ device, session, stream string }
type controllerAgent struct {
	conn *websocket.Conn
	mu   sync.Mutex // gorilla/websocket permits one concurrent writer only
}

type authorizedStream struct {
	conn       *websocket.Conn
	writeMu    sync.Mutex
	key        streamKey
	capability policy.Capability
	permission policy.Permission
	blind      bool
	browserIn  CounterTracker
	agentIn    CounterTracker
	closed     chan struct{}
	closeOnce  sync.Once
}

func NewController(store *EnrollmentStore, origin string) *Controller {
	if store == nil {
		store = NewEnrollmentStore()
	}
	return &Controller{
		Store: store, Policy: policy.NewMemory(), Origin: origin,
		CapabilityTTL: time.Minute, ControlLeaseTTL: 30 * time.Second,
		Upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		agents:   make(map[string]*controllerAgent), streams: make(map[streamKey]*authorizedStream),
		usedCapabilities: make(map[string]struct{}),
	}
}

func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/enroll":
		c.handleEnroll(w, r)
	case "/ws":
		c.handleWebSocket(w, r)
	case "/capabilities":
		c.handleCapability(w, r)
	case "/browser":
		c.handleBrowser(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (c *Controller) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req EnrollmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cred, err := c.Store.Consume(req.Code, req.PublicKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cred)
}

func (c *Controller) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := c.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	deviceID, hsErr := c.handshake(conn)
	if hsErr != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, hsErr.Error()), time.Now().Add(time.Second))
		return
	}
	if c.Policy != nil && c.Policy.IsDeviceRevoked(deviceID) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "device is revoked"), time.Now().Add(time.Second))
		return
	}
	// Register the authenticated agent and route opaque stream envelopes until
	// the connection closes. In blind mode the controller only decodes the
	// routing header; payload bytes are forwarded without inspection.
	agent := &controllerAgent{conn: conn}
	c.mu.Lock()
	if old := c.agents[deviceID]; old != nil && old.conn != conn {
		_ = old.conn.Close()
	}
	c.agents[deviceID] = agent
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.agents[deviceID] == agent {
			delete(c.agents, deviceID)
		}
		for key, peer := range c.streams {
			if key.device == deviceID {
				peer.close(websocket.CloseGoingAway, "agent disconnected")
				delete(c.streams, key)
			}
		}
		c.mu.Unlock()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		env, err := Decode(data)
		if err != nil || env.Header.DeviceID != deviceID {
			continue
		}
		key := streamKey{device: env.Header.DeviceID, session: env.Header.SessionID, stream: env.Header.StreamID}
		c.mu.Lock()
		peer := c.streams[key]
		if env.Header.FrameType == FrameClose {
			delete(c.streams, key)
		}
		c.mu.Unlock()
		if peer != nil {
			if err := peer.agentIn.Accept(env.Header.Counter); err != nil {
				peer.close(websocket.ClosePolicyViolation, "agent replay rejected")
				continue
			}
			if err := peer.write(data); err != nil {
				peer.close(websocket.CloseGoingAway, "browser disconnected")
			}
		}
	}
}

// handleBrowser upgrades an authenticated browser/native client and forwards
// its authorized stream to the outbound agent. OPEN carries a short-lived
// capability in its payload; capabilities are intentionally never accepted in
// a URL or cookie.
func (c *Controller) handleBrowser(w http.ResponseWriter, r *http.Request) {
	subject, err := c.browserSubject(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	conn, err := c.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_, first, err := conn.ReadMessage()
	if err != nil {
		return
	}
	env, err := Decode(first)
	if err != nil || env.Header.FrameType != FrameOpen || env.Header.DeviceID == "" || env.Header.SessionID == "" || env.Header.StreamID == "" {
		c.audit(policy.AuditEvent{Actor: subject, Action: "attach", Outcome: "denied", Reason: "invalid open envelope"})
		closeWebSocket(conn, websocket.ClosePolicyViolation, "invalid open envelope")
		return
	}
	var presented capabilityPresentation
	if err := json.Unmarshal(env.Payload, &presented); err != nil || presented.ID == "" || presented.Token == "" {
		c.audit(policy.AuditEvent{Actor: subject, DeviceID: env.Header.DeviceID, Session: env.Header.SessionID, Action: "attach", Outcome: "denied", Reason: "invalid capability"})
		closeWebSocket(conn, websocket.ClosePolicyViolation, "invalid capability")
		return
	}
	permission := policy.Permission(presented.Permission)
	blind := len(presented.Blind) > 0
	capability, err := c.Policy.AuthenticateCapability(presented.ID, presented.Token, permission, time.Now().UTC())
	if err != nil || capability.Subject != subject || capability.DeviceID != env.Header.DeviceID || capability.Session != env.Header.SessionID {
		reason := "capability binding mismatch"
		if err != nil {
			reason = err.Error()
		}
		c.audit(policy.AuditEvent{Actor: subject, DeviceID: env.Header.DeviceID, Session: env.Header.SessionID, Action: "attach", Capability: presented.ID, Outcome: "denied", Reason: reason})
		closeWebSocket(conn, websocket.ClosePolicyViolation, "attach denied")
		return
	}
	key := streamKey{device: env.Header.DeviceID, session: env.Header.SessionID, stream: env.Header.StreamID}
	c.mu.Lock()
	agent := c.agents[env.Header.DeviceID]
	_, duplicateStream := c.streams[key]
	_, used := c.usedCapabilities[capability.ID]
	if agent != nil && !duplicateStream && !used {
		c.usedCapabilities[capability.ID] = struct{}{}
	}
	c.mu.Unlock()
	if agent == nil || duplicateStream || used {
		reason := "agent is offline"
		if duplicateStream {
			reason = "stream already exists"
		} else if used {
			reason = "capability was already used"
		}
		c.audit(policy.AuditEvent{Actor: subject, DeviceID: key.device, Session: key.session, Action: "attach", Capability: capability.ID, Outcome: "denied", Reason: reason})
		closeWebSocket(conn, websocket.ClosePolicyViolation, reason)
		return
	}
	stream := &authorizedStream{conn: conn, key: key, capability: capability, permission: permission, blind: blind, closed: make(chan struct{})}
	if err := stream.browserIn.Accept(env.Header.Counter); err != nil {
		closeWebSocket(conn, websocket.ClosePolicyViolation, "invalid open counter")
		return
	}
	c.mu.Lock()
	c.streams[key] = stream
	c.mu.Unlock()
	c.audit(policy.AuditEvent{Actor: subject, DeviceID: key.device, Session: key.session, Action: "attach", RequestID: env.Header.RequestID, Capability: capability.ID, Outcome: "allowed"})
	defer func() {
		c.mu.Lock()
		if c.streams[key] == stream {
			delete(c.streams, key)
		}
		c.mu.Unlock()
		stream.closeOnce.Do(func() { close(stream.closed) })
		_ = c.Policy.ReleaseLease(key.session, subject)
		c.audit(policy.AuditEvent{Actor: subject, DeviceID: key.device, Session: key.session, Action: "detach", Capability: capability.ID, Outcome: "allowed"})
	}()
	go c.watchStreamAuthorization(stream)
	if err := controllerWrite(agent, first); err != nil {
		return
	}
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		env, err := Decode(data)
		if err != nil || env.Header.DeviceID != key.device || env.Header.SessionID != key.session || env.Header.StreamID != key.stream {
			stream.close(websocket.ClosePolicyViolation, "stream binding mismatch")
			return
		}
		if err := stream.browserIn.Accept(env.Header.Counter); err != nil {
			stream.close(websocket.ClosePolicyViolation, "browser replay rejected")
			return
		}
		if err := c.authorizeBrowserFrame(stream, env); err != nil {
			c.audit(policy.AuditEvent{Actor: subject, DeviceID: key.device, Session: key.session, Action: "session-frame", RequestID: env.Header.RequestID, Capability: capability.ID, Outcome: "denied", Reason: err.Error()})
			stream.close(websocket.ClosePolicyViolation, "session operation denied")
			return
		}
		if err := controllerWrite(agent, data); err != nil {
			return
		}
		if env.Header.FrameType == FrameClose {
			return
		}
	}
}

// BrowserCapability is the bearer value returned by POST /capabilities and
// presented once in the payload of the browser's first OPEN envelope.
type BrowserCapability struct {
	ID         string            `json:"id"`
	Token      string            `json:"token"`
	DeviceID   string            `json:"device_id"`
	SessionID  string            `json:"session_id"`
	Permission policy.Permission `json:"permission"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

type capabilityPresentation struct {
	ID         string            `json:"id"`
	Token      string            `json:"token"`
	Permission policy.Permission `json:"permission"`
	Blind      json.RawMessage   `json:"blind,omitempty"`
}

type capabilityRequest struct {
	DeviceID   string            `json:"device_id"`
	SessionID  string            `json:"session_id"`
	Permission policy.Permission `json:"permission"`
	TTLSeconds int64             `json:"ttl_seconds,omitempty"`
}

func (c *Controller) browserSubject(r *http.Request) (string, error) {
	if c.AuthenticateBrowser != nil {
		return c.AuthenticateBrowser(r)
	}
	subject := r.Header.Get("X-Shenmux-Subject")
	if subject == "" {
		return "", errors.New("browser authentication is required")
	}
	return subject, nil
}

func (c *Controller) handleCapability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	subject, err := c.browserSubject(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req capabilityRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.DeviceID == "" || req.SessionID == "" || (req.Permission != policy.PermissionObserve && req.Permission != policy.PermissionControl) {
		http.Error(w, "invalid capability request", http.StatusBadRequest)
		return
	}
	ttl := c.CapabilityTTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	if req.TTLSeconds > 0 {
		requested := time.Duration(req.TTLSeconds) * time.Second
		if requested < ttl {
			ttl = requested
		}
	}
	capability, err := c.Policy.IssueCapability(subject, req.DeviceID, req.SessionID, req.Permission, ttl)
	if err != nil {
		c.audit(policy.AuditEvent{Actor: subject, DeviceID: req.DeviceID, Session: req.SessionID, Action: "issue-capability", Outcome: "denied", Reason: err.Error()})
		http.Error(w, "capability denied", http.StatusForbidden)
		return
	}
	c.audit(policy.AuditEvent{Actor: subject, DeviceID: req.DeviceID, Session: req.SessionID, Action: "issue-capability", Capability: capability.ID, Outcome: "allowed"})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(BrowserCapability{
		ID: capability.ID, Token: capability.Token, DeviceID: capability.DeviceID,
		SessionID: capability.Session, Permission: req.Permission, ExpiresAt: capability.ExpiresAt,
	})
}

func (c *Controller) authorizeBrowserFrame(stream *authorizedStream, env Envelope) error {
	if c.Policy == nil {
		return errors.New("controller policy is unavailable")
	}
	if _, err := c.Policy.AuthenticateCapability(stream.capability.ID, stream.capability.Token, stream.permission, time.Now().UTC()); err != nil {
		return err
	}
	if env.Header.FrameType == FrameClose {
		return nil
	}
	if stream.blind {
		if env.Header.FrameType == FrameOpen {
			return nil
		}
		if stream.permission != policy.PermissionControl {
			return errors.New("blind data requires control capability")
		}
		// Blind payloads are opaque to the controller. The agent endpoint
		// validates the inner protocol and control lease after decryption.
		return nil
	}
	if env.Header.FrameType != FrameSession {
		return errors.New("unexpected browser frame type")
	}
	frames, err := UnmarshalFrames(env.Payload)
	if err != nil {
		return errors.New("invalid trusted session frame")
	}
	msg, err := protocol.Decode(frames)
	if err != nil {
		return errors.New("invalid trusted session frame")
	}
	if msg.Meta.Session != "" && msg.Meta.Session != stream.key.session {
		return errors.New("inner session binding mismatch")
	}
	now := time.Now().UTC()
	switch msg.Kind {
	case protocol.KindAttach, protocol.KindPing, protocol.KindResync, protocol.KindDetach:
		return nil
	case protocol.KindAcquireControl:
		if stream.permission != policy.PermissionControl {
			return errors.New("control capability required")
		}
		_, err := c.Policy.AcquireLease(stream.capability, c.controlLeaseTTL(), now)
		return err
	case protocol.KindReleaseControl:
		if stream.permission != policy.PermissionControl {
			return errors.New("control capability required")
		}
		lease, ok := c.Policy.Lease(stream.key.session)
		if !ok || lease.Capability != stream.capability.ID {
			return errors.New("control lease is not held")
		}
		return c.Policy.ReleaseLease(stream.key.session, stream.capability.Subject)
	case protocol.KindInput, protocol.KindResize:
		if stream.permission != policy.PermissionControl {
			return errors.New("control capability required")
		}
		lease, ok := c.Policy.Lease(stream.key.session)
		if !ok || lease.Capability != stream.capability.ID || !now.Before(lease.ExpiresAt) {
			return errors.New("control lease is not held")
		}
		_, err := c.Policy.RenewLease(stream.key.session, stream.capability.Subject, stream.capability.ID, c.controlLeaseTTL(), now)
		return err
	default:
		return errors.New("browser may not send this session message")
	}
}

func (c *Controller) controlLeaseTTL() time.Duration {
	if c.ControlLeaseTTL <= 0 {
		return 30 * time.Second
	}
	return c.ControlLeaseTTL
}

func (c *Controller) audit(event policy.AuditEvent) {
	if c.Policy != nil {
		_ = c.Policy.Audit(event)
	}
}

func (stream *authorizedStream) write(data []byte) error {
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()
	return stream.conn.WriteMessage(websocket.BinaryMessage, data)
}

func closeWebSocket(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = conn.Close()
}

func (stream *authorizedStream) close(code int, reason string) {
	stream.closeOnce.Do(func() {
		stream.writeMu.Lock()
		closeWebSocket(stream.conn, code, reason)
		stream.writeMu.Unlock()
		close(stream.closed)
	})
}

func (c *Controller) watchStreamAuthorization(stream *authorizedStream) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stream.closed:
			return
		case <-ticker.C:
			if _, err := c.Policy.AuthenticateCapability(stream.capability.ID, stream.capability.Token, stream.permission, time.Now().UTC()); err != nil {
				c.audit(policy.AuditEvent{Actor: stream.capability.Subject, DeviceID: stream.key.device, Session: stream.key.session, Action: "revoke-stream", Capability: stream.capability.ID, Outcome: "closed", Reason: err.Error()})
				stream.close(websocket.ClosePolicyViolation, "capability revoked")
				return
			}
		}
	}
}

// EnforceRevocations immediately revalidates all connected devices and
// browser streams. It is called by the controller revocation helpers and is
// also complemented by per-stream watchers for callers mutating Policy
// directly.
func (c *Controller) EnforceRevocations() {
	c.mu.Lock()
	var closeAgents []*controllerAgent
	var closeStreams []*authorizedStream
	for id, agent := range c.agents {
		if c.Policy.IsDeviceRevoked(id) {
			closeAgents = append(closeAgents, agent)
		}
	}
	for _, stream := range c.streams {
		if _, err := c.Policy.AuthenticateCapability(stream.capability.ID, stream.capability.Token, stream.permission, time.Now().UTC()); err != nil {
			closeStreams = append(closeStreams, stream)
		}
	}
	c.mu.Unlock()
	for _, stream := range closeStreams {
		stream.close(websocket.ClosePolicyViolation, "capability revoked")
	}
	for _, agent := range closeAgents {
		_ = agent.conn.Close()
	}
}

func (c *Controller) RevokeDevice(deviceID, reason string) error {
	if err := c.Policy.RevokeDevice(deviceID, reason); err != nil {
		return err
	}
	c.EnforceRevocations()
	return nil
}

func (c *Controller) RevokeSubject(subject, reason string) error {
	if err := c.Policy.RevokeSubject(subject, reason); err != nil {
		return err
	}
	c.EnforceRevocations()
	return nil
}

func (c *Controller) RevokeGrant(grantID, reason string) error {
	if err := c.Policy.RevokeGrant(grantID, reason); err != nil {
		return err
	}
	c.EnforceRevocations()
	return nil
}

func controllerWrite(agent *controllerAgent, data []byte) error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *Controller) handshake(conn *websocket.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	_, data, err := conn.ReadMessage()
	if err != nil {
		return "", err
	}
	env, err := Decode(data)
	if err != nil || env.Header.FrameType != FrameHello {
		return "", errors.New("expected hello")
	}
	var hello helloPayload
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		return "", errors.New("invalid hello payload")
	}
	pub, ok := c.Store.authenticate(hello.DeviceID, hello.Token)
	if !ok || len(hello.Nonce) == 0 {
		return "", errors.New("invalid device credential")
	}
	controllerNonce := make([]byte, 32)
	if _, err := rand.Read(controllerNonce); err != nil {
		return "", err
	}
	challenge, _ := json.Marshal(challengePayload{Nonce: controllerNonce})
	if err := writeEnvelope(conn, FrameChallenge, hello.DeviceID, 1, challenge); err != nil {
		return "", err
	}
	_, data, err = conn.ReadMessage()
	if err != nil {
		return "", err
	}
	proofEnv, err := Decode(data)
	if err != nil || proofEnv.Header.FrameType != FrameProof || proofEnv.Header.DeviceID != hello.DeviceID {
		return "", errors.New("expected challenge proof")
	}
	if err := VerifyChallenge(pub, proofEnv.Payload, Version, c.Origin, hello.DeviceID, hello.Nonce, controllerNonce); err != nil {
		return "", err
	}
	if err := writeEnvelope(conn, FrameReady, hello.DeviceID, 2, []byte(`{"version":1}`)); err != nil {
		return "", err
	}
	return hello.DeviceID, nil
}

func writeEnvelope(conn *websocket.Conn, frameType, deviceID string, counter uint64, payload []byte) error {
	data, err := (Envelope{Header: Header{FrameType: frameType, DeviceID: deviceID, Counter: counter}, Payload: payload}).Encode()
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

// Tunnel is an authenticated, outbound-only agent connection.
type Tunnel struct {
	URL string
	// Endpoints optionally lists direct/tailnet URLs followed by the relay URL.
	// URL remains the compatibility field and is used when Endpoints is empty.
	Endpoints  []string
	Origin     string
	Credential DeviceCredential
	PrivateKey ed25519.PrivateKey
	Dialer     *websocket.Dialer
	Policy     BackoffPolicy
}

func (t *Tunnel) Connect(ctx context.Context) (*websocket.Conn, error) {
	if err := t.Credential.Validate(time.Now()); err != nil {
		return nil, err
	}
	if len(t.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid device private key")
	}
	dialer := t.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	urls := t.Endpoints
	if len(urls) == 0 {
		urls = []string{t.URL}
	}
	var conn *websocket.Conn
	var err error
	for _, endpoint := range urls {
		if endpoint == "" {
			continue
		}
		conn, _, err = dialer.DialContext(ctx, endpoint, nil)
		if err != nil {
			continue
		}
		if err = t.handshake(ctx, conn); err == nil {
			return conn, nil
		}
		_ = conn.Close()
	}
	if err == nil {
		err = errors.New("no tunnel endpoints")
	}
	return nil, err
}

func (t *Tunnel) handshake(ctx context.Context, conn *websocket.Conn) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	agentNonce := make([]byte, 32)
	if _, err := rand.Read(agentNonce); err != nil {
		return err
	}
	hello, _ := json.Marshal(helloPayload{DeviceID: t.Credential.DeviceID, Token: t.Credential.Token, Origin: t.Origin, Nonce: agentNonce})
	if err := writeEnvelope(conn, FrameHello, t.Credential.DeviceID, 1, hello); err != nil {
		return err
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	env, err := Decode(data)
	if err != nil || env.Header.FrameType != FrameChallenge || env.Header.DeviceID != t.Credential.DeviceID {
		return errors.New("expected challenge")
	}
	var challenge challengePayload
	if err := json.Unmarshal(env.Payload, &challenge); err != nil || len(challenge.Nonce) == 0 {
		return errors.New("invalid challenge payload")
	}
	sig, err := SignChallenge(t.PrivateKey, Version, t.Origin, t.Credential.DeviceID, agentNonce, challenge.Nonce)
	if err != nil {
		return err
	}
	if err := writeEnvelope(conn, FrameProof, t.Credential.DeviceID, 2, sig); err != nil {
		return err
	}
	_, data, err = conn.ReadMessage()
	if err != nil {
		return err
	}
	ready, err := Decode(data)
	if err != nil || ready.Header.FrameType != FrameReady || ready.Header.DeviceID != t.Credential.DeviceID {
		return errors.New("expected ready")
	}
	_ = conn.SetReadDeadline(time.Time{})
	return nil
}

func (t *Tunnel) Run(ctx context.Context, connected func(*websocket.Conn) error) error {
	if connected == nil {
		return errors.New("connected callback must not be nil")
	}
	reconnector := NewReconnector(t.Policy)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		reconnector.Start()
		conn, err := t.Connect(ctx)
		if err == nil {
			reconnector.Connected()
			err = connected(conn)
			_ = conn.Close()
			if err == nil {
				return nil
			}
		}
		delay := reconnector.Failed()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
