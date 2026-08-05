package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pyrex41/shenmux/internal/policy"
	"github.com/pyrex41/shenmux/internal/protocol"
)

func TestEnvelopeMessageRoundTripPreservesFrames(t *testing.T) {
	msg := protocol.Message{Kind: protocol.KindDelta, Meta: protocol.Meta{Version: protocol.Version, Session: "s", Seq: 9}, Payload: []byte{0, 1, 2}}
	frames, err := protocol.Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := MarshalFrames(frames)
	if err != nil {
		t.Fatal(err)
	}
	data, err := (Envelope{Header: Header{FrameType: "session", DeviceID: "d", SessionID: "s", StreamID: "x", Counter: 4}, Payload: inner}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	env, got, err := DecodeMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	if env.Header.Version != Version || env.Header.Counter != 4 {
		t.Fatalf("unexpected header: %+v", env.Header)
	}
	if got.Kind != msg.Kind || got.Meta != msg.Meta || !bytes.Equal(got.Payload, msg.Payload) {
		t.Fatalf("message changed: %#v", got)
	}
	decodedFrames, err := UnmarshalFrames(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for i := range frames {
		if !bytes.Equal(frames[i], decodedFrames[i]) {
			t.Fatalf("frame %d changed", i)
		}
	}
}

func TestPersistentEnrollmentStoreSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/enrollment.json"
	store, err := NewPersistentEnrollmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := store.Consume(code, pub)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewPersistentEnrollmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.authenticate(credential.DeviceID, credential.Token); !ok {
		t.Fatal("device credential was not persisted")
	}
	if _, err := reopened.Consume(code, pub); err == nil {
		t.Fatal("consumed enrollment code was reusable after restart")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("enrollment file permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestEnvelopeRejectsMalformedAndOversizedHeaders(t *testing.T) {
	if _, err := Decode([]byte{0, 0, 0}); err == nil {
		t.Fatal("expected short envelope error")
	}
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, MaxHeaderSize+1)
	if _, err := Decode(data); err == nil {
		t.Fatal("expected oversized header error")
	}
	if _, err := MarshalFrames([][]byte{[]byte("one")}); err == nil {
		t.Fatal("expected frame count error")
	}
}

func TestCounterTrackerRejectsReplay(t *testing.T) {
	var tracker CounterTracker
	if err := tracker.Accept(7); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Accept(7); err == nil {
		t.Fatal("expected duplicate counter rejection")
	}
	if err := tracker.Accept(6); err == nil {
		t.Fatal("expected counter rollback rejection")
	}
	if err := tracker.Accept(8); err != nil {
		t.Fatal(err)
	}
	if got, ok := tracker.Last(); !ok || got != 8 {
		t.Fatalf("last counter = %d, %v", got, ok)
	}
}

func TestChallengeProof(t *testing.T) {
	code, err := NewEnrollmentCode()
	if err != nil || len(code) < 20 {
		t.Fatalf("enrollment code: %q, %v", code, err)
	}
	if HashEnrollmentCode(code) == HashEnrollmentCode(code+"x") {
		t.Fatal("enrollment code hash collision")
	}
	pub, priv, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignChallenge(priv, Version, "https://controller", "device-a", []byte("agent"), []byte("controller"))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChallenge(pub, sig, Version, "https://controller", "device-a", []byte("agent"), []byte("controller")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChallenge(pub, sig, Version, "https://evil", "device-a", []byte("agent"), []byte("controller")); err == nil {
		t.Fatal("expected origin binding failure")
	}
	if err := VerifyChallenge(ed25519.PublicKey(pub), sig[:len(sig)-1], Version, "https://controller", "device-a", []byte("agent"), []byte("controller")); err == nil {
		t.Fatal("expected signature length failure")
	}
}

func TestBackoffAndReconnectState(t *testing.T) {
	p := BackoffPolicy{Initial: 100 * time.Millisecond, Maximum: 500 * time.Millisecond, Factor: 2}
	r := NewReconnector(p)
	if r.State() != StateDisconnected {
		t.Fatal("initial state")
	}
	r.Start()
	if r.State() != StateConnecting {
		t.Fatal("connecting state")
	}
	if d := r.Failed(); d != 100*time.Millisecond {
		t.Fatalf("first delay %v", d)
	}
	r.Start()
	if d := r.Failed(); d != 200*time.Millisecond {
		t.Fatalf("second delay %v", d)
	}
	r.Start()
	if d := r.Failed(); d != 400*time.Millisecond {
		t.Fatalf("third delay %v", d)
	}
	r.Start()
	if d := r.Failed(); d != 500*time.Millisecond {
		t.Fatalf("bounded delay %v", d)
	}
	r.Connected()
	if r.Attempt() != 0 || r.State() != StateConnected {
		t.Fatal("successful connection did not reset state")
	}
}

func TestBackoffJitterRemainsBounded(t *testing.T) {
	p := BackoffPolicy{Initial: time.Second, Maximum: 2 * time.Second, Factor: 2, Jitter: 1, Rand: rand.New(rand.NewSource(1))}
	for i := 0; i < 32; i++ {
		if got := p.Delay(3); got < 0 || got > p.Maximum {
			t.Fatalf("jittered delay escaped bounds: %v", got)
		}
	}
}

func TestAuthenticatedTunnelEnrollmentAndHandshake(t *testing.T) {
	store := NewEnrollmentStore()
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, "test-controller")
	httpServer := httptest.NewServer(controller)
	defer httpServer.Close()
	pub, priv, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(EnrollmentRequest{Code: code, PublicKey: pub})
	resp, err := http.Post(httpServer.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enrollment status %d", resp.StatusCode)
	}
	var cred DeviceCredential
	if err := json.NewDecoder(resp.Body).Decode(&cred); err != nil {
		t.Fatal(err)
	}
	wsURL, _ := url.Parse(httpServer.URL)
	wsURL.Scheme = "ws"
	tunnel := Tunnel{URL: wsURL.String() + "/ws", Origin: "test-controller", Credential: cred, PrivateKey: priv, Metadata: AgentMetadata{Cluster: "dev", Namespace: "agents", Workload: "codex", Pod: "codex-0", Harness: "codex", Sessions: []SessionDescriptor{{ID: "shell", Name: "shell", Kind: "harness", Interactive: true}}}, Policy: BackoffPolicy{Initial: time.Millisecond, Maximum: time.Millisecond}}
	conn, err := tunnel.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/sessions?namespace=agents", nil)
	request.Header.Set("X-Shenmux-Subject", "alice")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session discovery status %d", response.StatusCode)
	}
	var discovered []DiscoveredSession
	if err := json.NewDecoder(response.Body).Decode(&discovered); err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].Session.ID != "shell" || discovered[0].Metadata.Pod != "codex-0" {
		t.Fatalf("discovered sessions = %+v", discovered)
	}
	conn.Close()
	// Enrollment codes are atomically single-use.
	body, _ = json.Marshal(EnrollmentRequest{Code: code, PublicKey: pub})
	resp, err = http.Post(httpServer.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed enrollment status %d", resp.StatusCode)
	}
}

func TestTunnelRunRecoversAfterControllerOutage(t *testing.T) {
	store := NewEnrollmentStore()
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, "outage-controller")
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			// Simulate a short controller outage before the endpoint becomes
			// reachable. WebSocket clients observe these as dial failures.
			if attempts.Add(1) <= 2 {
				http.Error(w, "controller unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		controller.ServeHTTP(w, r)
	}))
	defer server.Close()

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
	wsURL, _ := url.Parse(server.URL)
	wsURL.Scheme = "ws"
	tunnel := Tunnel{URL: wsURL.String() + "/ws", Origin: "outage-controller", Credential: credential, PrivateKey: priv,
		Policy: BackoffPolicy{Initial: 5 * time.Millisecond, Maximum: 20 * time.Millisecond, Factor: 2}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var connected atomic.Int32
	err = tunnel.Run(ctx, func(conn *websocket.Conn) error {
		if connected.Add(1) == 1 {
			return errors.New("force reconnect")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got < 4 {
		t.Fatalf("expected outage retries then recovery, dial attempts=%d", got)
	}
	if got := connected.Load(); got != 2 {
		t.Fatalf("expected two authenticated connections, got %d", got)
	}
}

func TestControllerRoutesOpaqueBrowserStream(t *testing.T) {
	store := NewEnrollmentStore()
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, "test-controller")
	if _, err := controller.Policy.AddGrant("alice", "", "shell", []policy.Permission{policy.PermissionObserve}, nil); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(controller)
	defer httpServer.Close()
	pub, priv, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(EnrollmentRequest{Code: code, PublicKey: pub})
	resp, err := http.Post(httpServer.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cred DeviceCredential
	if err := json.NewDecoder(resp.Body).Decode(&cred); err != nil {
		t.Fatal(err)
	}
	wsURL, _ := url.Parse(httpServer.URL)
	wsURL.Scheme = "ws"
	tunnel := Tunnel{URL: wsURL.String() + "/ws", Origin: "test-controller", Credential: cred, PrivateKey: priv}
	agentConn, err := tunnel.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer agentConn.Close()
	// Controller registration completes when the handshake returns. Give the
	// handler a scheduling turn before opening the browser stream.
	time.Sleep(10 * time.Millisecond)
	capRequest, _ := json.Marshal(map[string]any{"device_id": cred.DeviceID, "session_id": "shell", "permission": "observe"})
	req, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/capabilities", bytes.NewReader(capRequest))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shenmux-Subject", "alice")
	capResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer capResp.Body.Close()
	var capability BrowserCapability
	if err := json.NewDecoder(capResp.Body).Decode(&capability); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"X-Shenmux-Subject": []string{"alice"}}
	browserConn, _, err := websocket.DefaultDialer.Dial(wsURL.String()+"/browser", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer browserConn.Close()
	openPayload, _ := json.Marshal(map[string]any{"id": capability.ID, "token": capability.Token, "permission": capability.Permission, "mode": "blind"})
	open := Envelope{Header: Header{FrameType: FrameOpen, DeviceID: cred.DeviceID, SessionID: "shell", StreamID: "stream-1", Counter: 1}, Payload: openPayload}
	openBytes, err := open.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := browserConn.WriteMessage(websocket.BinaryMessage, openBytes); err != nil {
		t.Fatal(err)
	}
	_, gotOpen, err := agentConn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotOpen, openBytes) {
		t.Fatalf("open changed in relay")
	}
	// Blind payloads are ciphertext to the controller; forwarding must preserve
	// bytes exactly while routing from agent back to browser.
	ciphertext := []byte{0x93, 0x7a, 0x01, 0xff, 0x00, 0x44}
	response := Envelope{Header: Header{FrameType: FrameData, DeviceID: cred.DeviceID, SessionID: "shell", StreamID: "stream-1", Counter: 2}, Payload: ciphertext}
	responseBytes, err := response.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := agentConn.WriteMessage(websocket.BinaryMessage, responseBytes); err != nil {
		t.Fatal(err)
	}
	_, gotResponse, err := browserConn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotResponse, responseBytes) {
		t.Fatalf("session payload changed in relay")
	}
}

func TestTunnelFallsBackWhenDirectHandshakeFails(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = writeEnvelope(conn, FrameClose, "device-1", 1, []byte("bad direct path"))
	}))
	defer bad.Close()

	store := NewEnrollmentStore()
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, "test-controller")
	good := httptest.NewServer(controller)
	defer good.Close()
	pub, priv, err := GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(EnrollmentRequest{Code: code, PublicKey: pub})
	resp, err := http.Post(good.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var credential DeviceCredential
	if err := json.NewDecoder(resp.Body).Decode(&credential); err != nil {
		t.Fatal(err)
	}
	goodURL, _ := url.Parse(good.URL)
	goodURL.Scheme = "ws"
	badURL, _ := url.Parse(bad.URL)
	badURL.Scheme = "ws"
	tunnel := Tunnel{Endpoints: []string{badURL.String() + "/ws", goodURL.String() + "/ws"}, Origin: "test-controller", Credential: credential, PrivateKey: priv}
	conn, err := tunnel.Connect(context.Background())
	if err != nil {
		t.Fatalf("fallback connect failed: %v", err)
	}
	conn.Close()
}
