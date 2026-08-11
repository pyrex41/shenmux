package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pyrex41/shenmux/internal/policy"
	"github.com/pyrex41/shenmux/internal/protocol"
)

func TestControllerHealthz(t *testing.T) {
	server := httptest.NewServer(NewController(nil, "test-controller"))
	defer server.Close()
	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
}

type policyControllerFixture struct {
	controller *Controller
	server     *httptest.Server
	wsBase     string
	credential DeviceCredential
	agent      *websocket.Conn
}

func newPolicyControllerFixture(t *testing.T) *policyControllerFixture {
	t.Helper()
	store := NewEnrollmentStore()
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, "test-controller")
	server := httptest.NewServer(controller)
	t.Cleanup(server.Close)
	pub, priv, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(EnrollmentRequest{Code: code, PublicKey: pub})
	resp, err := http.Post(server.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var credential DeviceCredential
	if err := json.NewDecoder(resp.Body).Decode(&credential); err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(server.URL)
	parsed.Scheme = "ws"
	tunnel := Tunnel{URL: parsed.String() + "/ws", Origin: "test-controller", Credential: credential, PrivateKey: priv}
	agent, err := tunnel.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		controller.mu.Lock()
		registered := controller.agents[credential.DeviceID] != nil
		controller.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not register with controller")
		}
		time.Sleep(time.Millisecond)
	}
	return &policyControllerFixture{controller: controller, server: server, wsBase: parsed.String(), credential: credential, agent: agent}
}

func (f *policyControllerFixture) issueCapability(t *testing.T, subject, session string, permission policy.Permission) (BrowserCapability, int) {
	t.Helper()
	body, _ := json.Marshal(capabilityRequest{DeviceID: f.credential.DeviceID, SessionID: session, Permission: permission})
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/capabilities", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if subject != "" {
		req.Header.Set("X-Shenmux-Subject", subject)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var capability BrowserCapability
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&capability); err != nil {
			t.Fatal(err)
		}
	}
	return capability, resp.StatusCode
}

func (f *policyControllerFixture) openBrowser(t *testing.T, subject, streamID string, capability BrowserCapability) *websocket.Conn {
	t.Helper()
	headers := http.Header{"X-Shenmux-Subject": []string{subject}}
	conn, _, err := websocket.DefaultDialer.Dial(f.wsBase+"/browser", headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	presentation, _ := json.Marshal(capabilityPresentation{ID: capability.ID, Token: capability.Token, Permission: capability.Permission})
	data, err := (Envelope{Header: Header{
		FrameType: FrameOpen, DeviceID: capability.DeviceID, SessionID: capability.SessionID,
		StreamID: streamID, Counter: 1,
	}, Payload: presentation}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
	_ = f.agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, forwarded, err := f.agent.ReadMessage()
	_ = f.agent.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("agent did not receive authorized OPEN: %v", err)
	}
	if !bytes.Equal(forwarded, data) {
		t.Fatal("controller changed authorized OPEN")
	}
	return conn
}

// readAgentFrameFor returns the next envelope the controller forwards to the
// agent for streamID, together with its raw bytes. Stream teardown runs
// concurrently with forwarding, so a close belonging to another stream may
// interleave at any point; callers that care about those assert them
// explicitly with readAgentCloseFor.
func (f *policyControllerFixture) readAgentFrameFor(t *testing.T, streamID string) (Envelope, []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = f.agent.SetReadDeadline(deadline)
		_, data, err := f.agent.ReadMessage()
		_ = f.agent.SetReadDeadline(time.Time{})
		if err != nil {
			t.Fatalf("agent did not receive a frame for stream %q: %v", streamID, err)
		}
		env, err := Decode(data)
		if err != nil {
			t.Fatalf("agent received an undecodable frame: %v", err)
		}
		if env.Header.StreamID == streamID {
			return env, data
		}
	}
}

// readAgentCloseFor asserts that the controller told the agent streamID is
// over. Every browser stream must produce exactly one of these, whatever ended
// it, because the agent releases the control lease and detaches its muxd client
// on hearing it.
func (f *policyControllerFixture) readAgentCloseFor(t *testing.T, streamID string) Envelope {
	t.Helper()
	env, _ := f.readAgentFrameFor(t, streamID)
	if env.Header.FrameType != FrameClose {
		t.Fatalf("frame for stream %q = %q, want %q", streamID, env.Header.FrameType, FrameClose)
	}
	if env.Header.DeviceID != f.credential.DeviceID {
		t.Fatalf("close device = %q, want %q", env.Header.DeviceID, f.credential.DeviceID)
	}
	return env
}

// expectNoFurtherAgentFrames fails if the controller sends anything else within
// a short window. It is the duplicate-close guard.
func (f *policyControllerFixture) expectNoFurtherAgentFrames(t *testing.T) {
	t.Helper()
	_ = f.agent.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	_, data, err := f.agent.ReadMessage()
	_ = f.agent.SetReadDeadline(time.Time{})
	if err == nil {
		env, _ := Decode(data)
		t.Fatalf("controller sent an extra frame to the agent: %+v", env.Header)
	}
	if !isTimeout(err) {
		t.Fatalf("agent read failed for an unexpected reason: %v", err)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// waitForStreamRemoval blocks until the controller has finished the local half
// of its teardown, so a following read is not racing the handler's defer.
func (f *policyControllerFixture) waitForStreamRemoval(t *testing.T, session, streamID string) {
	t.Helper()
	key := streamKey{device: f.credential.DeviceID, session: session, stream: streamID}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.controller.mu.Lock()
		_, present := f.controller.streams[key]
		f.controller.mu.Unlock()
		if !present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller kept stream %q after the browser went away", streamID)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestControllerTellsAgentWhenBrowserVanishes covers the ordinary way a browser
// leaves: the tab is closed and the socket dies with no CLOSE frame. Nothing
// downstream -- the agent bridge releasing the lease, the reducer ageing
// ownership -- can start until the controller says so.
func TestControllerTellsAgentWhenBrowserVanishes(t *testing.T) {
	f := newPolicyControllerFixture(t)
	if _, err := f.controller.Policy.AddGrant("alice", f.credential.DeviceID, "shell", []policy.Permission{policy.PermissionControl}, nil); err != nil {
		t.Fatal(err)
	}
	capability, _ := f.issueCapability(t, "alice", "shell", policy.PermissionControl)
	browser := f.openBrowser(t, "alice", "vanishing-stream", capability)

	acquire := sessionEnvelope(t, f.credential.DeviceID, "shell", "vanishing-stream", 2, protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "shell", RequestID: 3}})
	if err := browser.WriteMessage(websocket.BinaryMessage, acquire); err != nil {
		t.Fatal(err)
	}
	if env, _ := f.readAgentFrameFor(t, "vanishing-stream"); env.Header.FrameType != FrameSession {
		t.Fatalf("acquire frame = %q", env.Header.FrameType)
	}
	if _, held := f.controller.Policy.Lease("shell"); !held {
		t.Fatal("control lease was not taken")
	}

	// Kill the transport underneath the WebSocket: no CLOSE frame, no close
	// handshake, exactly what a closed tab looks like from the controller.
	if err := browser.UnderlyingConn().Close(); err != nil {
		t.Fatal(err)
	}

	closed := f.readAgentCloseFor(t, "vanishing-stream")
	if closed.Header.SessionID != "shell" {
		t.Fatalf("close session = %q", closed.Header.SessionID)
	}
	if closed.Header.Counter <= 2 {
		t.Fatalf("close counter %d did not continue the browser->agent sequence", closed.Header.Counter)
	}
	f.waitForStreamRemoval(t, "shell", "vanishing-stream")
	if _, held := f.controller.Policy.Lease("shell"); held {
		t.Fatal("control lease survived the browser disappearing")
	}
	f.expectNoFurtherAgentFrames(t)
}

// TestControllerForwardsBrowserCloseOnce pins the idempotence half: a clean
// CLOSE is relayed byte for byte and teardown must not append a second one.
func TestControllerForwardsBrowserCloseOnce(t *testing.T) {
	f := newPolicyControllerFixture(t)
	if _, err := f.controller.Policy.AddGrant("alice", f.credential.DeviceID, "shell", []policy.Permission{policy.PermissionObserve}, nil); err != nil {
		t.Fatal(err)
	}
	capability, _ := f.issueCapability(t, "alice", "shell", policy.PermissionObserve)
	browser := f.openBrowser(t, "alice", "polite-stream", capability)

	sent, err := (Envelope{Header: Header{FrameType: FrameClose, DeviceID: f.credential.DeviceID, SessionID: "shell", StreamID: "polite-stream", Counter: 2}, Payload: []byte("bye")}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteMessage(websocket.BinaryMessage, sent); err != nil {
		t.Fatal(err)
	}
	_, forwarded := f.readAgentFrameFor(t, "polite-stream")
	if !bytes.Equal(forwarded, sent) {
		t.Fatal("controller changed the browser's CLOSE")
	}
	f.waitForStreamRemoval(t, "shell", "polite-stream")
	f.expectNoFurtherAgentFrames(t)
}

func sessionEnvelope(t *testing.T, device, session, stream string, counter uint64, msg protocol.Message) []byte {
	t.Helper()
	frames, err := protocol.Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalFrames(frames)
	if err != nil {
		t.Fatal(err)
	}
	data, err := (Envelope{Header: Header{FrameType: FrameSession, DeviceID: device, SessionID: session, StreamID: stream, Counter: counter, RequestID: msg.Meta.RequestID}, Payload: payload}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestControllerCapabilityEndpointAuthorization(t *testing.T) {
	f := newPolicyControllerFixture(t)
	if _, status := f.issueCapability(t, "", "shell", policy.PermissionObserve); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated capability status = %d", status)
	}
	if _, status := f.issueCapability(t, "alice", "shell", policy.PermissionObserve); status != http.StatusForbidden {
		t.Fatalf("ungranted capability status = %d", status)
	}
	if _, err := f.controller.Policy.AddGrant("alice", f.credential.DeviceID, "shell", []policy.Permission{policy.PermissionObserve}, nil); err != nil {
		t.Fatal(err)
	}
	capability, status := f.issueCapability(t, "alice", "shell", policy.PermissionObserve)
	if status != http.StatusOK || capability.ID == "" || capability.Token == "" || capability.DeviceID != f.credential.DeviceID {
		t.Fatalf("issued capability = %#v, status %d", capability, status)
	}
	if strings.Contains(capability.ID, capability.Token) {
		t.Fatal("capability ID unexpectedly embeds bearer token")
	}
	audit := f.controller.Policy.AuditEvents(10)
	if len(audit) != 2 || audit[0].Action != "issue-capability" || audit[0].Outcome != "denied" || audit[1].Outcome != "allowed" {
		t.Fatalf("unexpected capability audit: %#v", audit)
	}
}

func TestControllerAttachLeaseAndMetadataOnlyAudit(t *testing.T) {
	f := newPolicyControllerFixture(t)
	f.controller.ControlLeaseTTL = time.Second
	for _, subject := range []string{"alice", "bob"} {
		if _, err := f.controller.Policy.AddGrant(subject, f.credential.DeviceID, "shell", []policy.Permission{policy.PermissionControl}, nil); err != nil {
			t.Fatal(err)
		}
	}
	aliceCap, _ := f.issueCapability(t, "alice", "shell", policy.PermissionControl)
	bobCap, _ := f.issueCapability(t, "bob", "shell", policy.PermissionControl)
	alice := f.openBrowser(t, "alice", "alice-stream", aliceCap)
	bob := f.openBrowser(t, "bob", "bob-stream", bobCap)

	acquireAlice := sessionEnvelope(t, f.credential.DeviceID, "shell", "alice-stream", 2, protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "shell", RequestID: 7}})
	if err := alice.WriteMessage(websocket.BinaryMessage, acquireAlice); err != nil {
		t.Fatal(err)
	}
	if env, _ := f.readAgentFrameFor(t, "alice-stream"); env.Header.FrameType != FrameSession {
		t.Fatalf("authorized acquire was not forwarded: %+v", env.Header)
	}

	acquireBob := sessionEnvelope(t, f.credential.DeviceID, "shell", "bob-stream", 2, protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "shell", RequestID: 8}})
	if err := bob.WriteMessage(websocket.BinaryMessage, acquireBob); err != nil {
		t.Fatal(err)
	}
	_ = bob.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := bob.ReadMessage(); err == nil {
		t.Fatal("second controller received control while lease was held")
	}
	// Denying bob's acquire ends his stream, so the agent is owed a close for
	// it. It races alice's traffic on the shared tunnel, hence readAgentFrameFor.
	f.readAgentCloseFor(t, "bob-stream")

	secret := "do-not-audit-terminal-input"
	input := sessionEnvelope(t, f.credential.DeviceID, "shell", "alice-stream", 3, protocol.Message{Kind: protocol.KindInput, Meta: protocol.Meta{Version: protocol.Version, Session: "shell", RequestID: 9}, Payload: []byte(secret)})
	if err := alice.WriteMessage(websocket.BinaryMessage, input); err != nil {
		t.Fatal(err)
	}
	if _, got := f.readAgentFrameFor(t, "alice-stream"); !bytes.Equal(got, input) {
		t.Fatal("leased input not forwarded byte-for-byte")
	}
	auditJSON, _ := json.Marshal(f.controller.Policy.AuditEvents(100))
	if bytes.Contains(auditJSON, []byte(secret)) {
		t.Fatal("audit log contains terminal input")
	}
}

func TestControllerRevocationClosesLiveStreamsAndAgent(t *testing.T) {
	f := newPolicyControllerFixture(t)
	grant, err := f.controller.Policy.AddGrant("alice", f.credential.DeviceID, "shell", []policy.Permission{policy.PermissionObserve}, nil)
	if err != nil {
		t.Fatal(err)
	}
	capability, _ := f.issueCapability(t, "alice", "shell", policy.PermissionObserve)
	browser := f.openBrowser(t, "alice", "revoked-stream", capability)
	if err := f.controller.RevokeGrant(grant.ID, "access removed"); err != nil {
		t.Fatal(err)
	}
	_ = browser.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := browser.ReadMessage(); err == nil {
		t.Fatal("browser stream remained open after grant revocation")
	}
	// A revoked stream is still a stream that ended: the agent must be told, or
	// it keeps serving a browser the controller has already cut off.
	f.readAgentCloseFor(t, "revoked-stream")

	// Device revocation also closes the authenticated agent tunnel and prevents
	// the same credential from reconnecting.
	if err := f.controller.RevokeDevice(f.credential.DeviceID, "compromised"); err != nil {
		t.Fatal(err)
	}
	_ = f.agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := f.agent.ReadMessage(); err == nil {
		t.Fatal("agent tunnel remained open after device revocation")
	}
	audit, _ := json.Marshal(f.controller.Policy.AuditEvents(100))
	if bytes.Contains(audit, []byte("terminal")) {
		t.Fatal("revocation audit unexpectedly contains terminal contents")
	}
}

// Ensure test helpers don't accidentally rely on response bodies remaining
// unread, which can hide controller handler failures on reused HTTP clients.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
