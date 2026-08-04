package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	_ = f.agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := f.agent.ReadMessage(); err != nil {
		t.Fatalf("authorized acquire was not forwarded: %v", err)
	}
	_ = f.agent.SetReadDeadline(time.Time{})

	acquireBob := sessionEnvelope(t, f.credential.DeviceID, "shell", "bob-stream", 2, protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "shell", RequestID: 8}})
	if err := bob.WriteMessage(websocket.BinaryMessage, acquireBob); err != nil {
		t.Fatal(err)
	}
	_ = bob.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := bob.ReadMessage(); err == nil {
		t.Fatal("second controller received control while lease was held")
	}

	secret := "do-not-audit-terminal-input"
	input := sessionEnvelope(t, f.credential.DeviceID, "shell", "alice-stream", 3, protocol.Message{Kind: protocol.KindInput, Meta: protocol.Meta{Version: protocol.Version, Session: "shell", RequestID: 9}, Payload: []byte(secret)})
	if err := alice.WriteMessage(websocket.BinaryMessage, input); err != nil {
		t.Fatal(err)
	}
	_ = f.agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, got, err := f.agent.ReadMessage()
	_ = f.agent.SetReadDeadline(time.Time{})
	if err != nil || !bytes.Equal(got, input) {
		t.Fatalf("leased input not forwarded byte-for-byte: %v", err)
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
