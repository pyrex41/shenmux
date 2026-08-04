package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
)

func deterministicBlindPair(t *testing.T, binding StreamBinding) (*StreamCipher, *StreamCipher, BlindClientHello, BlindAgentResponse, BlindClientFinish) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	identity := ed25519.NewKeyFromSeed(seed)
	clientRandom := bytes.NewReader(bytes.Repeat([]byte{0x42}, 32))
	agentRandom := bytes.NewReader(bytes.Repeat([]byte{0x24}, 32))
	clientHandshake, hello, err := NewBlindClientHandshake(binding, clientRandom)
	if err != nil {
		t.Fatal(err)
	}
	agentHandshake, response, err := AcceptBlindClientHello(binding, hello, identity, TrustBlind, agentRandom)
	if err != nil {
		t.Fatal(err)
	}
	clientCipher, finish, err := clientHandshake.Finish(response, identity.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	agentCipher, err := agentHandshake.Confirm(finish)
	if err != nil {
		t.Fatal(err)
	}
	return clientCipher, agentCipher, hello, response, finish
}

func testBlindBinding(t *testing.T) StreamBinding {
	t.Helper()
	binding, err := NewStreamBinding("device-7", "shell", "stream-9", "user-3", "control", []byte("signed-capability-token"))
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestBlindHandshakeAndDirectionalEncryption(t *testing.T) {
	binding := testBlindBinding(t)
	client, agent, _, _, _ := deterministicBlindPair(t, binding)
	header := Header{Version: Version, FrameType: FrameSession, RequestID: 11, DeviceID: binding.DeviceID, SessionID: binding.SessionID, StreamID: binding.StreamID, Counter: 1}
	plaintext := []byte("full checkpoint and terminal bytes")
	ciphertext, err := client.Seal(header, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plaintext) || bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext exposed plaintext")
	}
	got, err := agent.Open(header, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext changed: %q", got)
	}

	responseHeader := header
	responseHeader.Counter = 1
	responseHeader.RequestID = 12
	response, err := agent.Seal(responseHeader, []byte("screen-delta"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = client.Open(responseHeader, response)
	if err != nil || string(got) != "screen-delta" {
		t.Fatalf("agent-to-client open = %q, %v", got, err)
	}

	inner := []byte{0, 0, 0, 2, 0, 0, 0, 5, 'i', 'n', 'p', 'u', 't'}
	encrypted, err := client.SealEnvelope(Envelope{Header: Header{Version: Version, FrameType: FrameSession, DeviceID: binding.DeviceID, SessionID: binding.SessionID, StreamID: binding.StreamID, Counter: 2}, Payload: inner})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := agent.OpenEnvelope(encrypted)
	if err != nil || !bytes.Equal(opened.Payload, inner) {
		t.Fatalf("envelope payload = %x, %v", opened.Payload, err)
	}
}

func TestBlindStreamSessionOpenData(t *testing.T) {
	binding := testBlindBinding(t)
	seed := bytes.Repeat([]byte{0x41}, ed25519.SeedSize)
	identity := ed25519.NewKeyFromSeed(seed)
	client, open, err := NewBlindClientStream(binding, identity.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	open.Header.Counter = 1
	agent, err := NewBlindAgentStream(binding, identity, TrustBlind)
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.HandleOpen(open)
	if err != nil {
		t.Fatal(err)
	}
	response.Header.Counter = 1
	finish, err := client.HandleOpen(response)
	if err != nil {
		t.Fatal(err)
	}
	finish.Header.Counter = 2
	if _, err := agent.HandleOpen(finish); err != nil {
		t.Fatal(err)
	}
	if !client.Ready() || !agent.Ready() {
		t.Fatal("blind stream handshake did not complete")
	}
	data := Envelope{Header: Header{FrameType: FrameData, DeviceID: binding.DeviceID, SessionID: binding.SessionID, StreamID: binding.StreamID, Counter: 1}, Payload: []byte("checkpoint bytes")}
	encrypted, err := client.SealData(data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.Payload, data.Payload) {
		t.Fatal("blind DATA exposed plaintext")
	}
	decrypted, err := agent.OpenData(encrypted)
	if err != nil || !bytes.Equal(decrypted.Payload, data.Payload) {
		t.Fatalf("agent DATA decrypt = %q, %v", decrypted.Payload, err)
	}
}

func TestBlindReplayTamperAndRoutingProtection(t *testing.T) {
	binding := testBlindBinding(t)
	client, agent, _, _, _ := deterministicBlindPair(t, binding)
	header := Header{Version: Version, FrameType: FrameSession, RequestID: 41, DeviceID: binding.DeviceID, SessionID: binding.SessionID, StreamID: binding.StreamID, Counter: 1}
	ciphertext, err := client.Seal(header, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0x80
	if _, err := agent.Open(header, tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	// Authentication failure must not consume the counter.
	if _, err := agent.Open(header, ciphertext); err != nil {
		t.Fatalf("valid packet after tamper: %v", err)
	}
	if _, err := agent.Open(header, ciphertext); err == nil {
		t.Fatal("replayed ciphertext was accepted")
	}

	header.Counter = 2
	ciphertext, err = client.Seal(header, []byte("metadata-bound"))
	if err != nil {
		t.Fatal(err)
	}
	changed := header
	changed.RequestID++
	if _, err := agent.Open(changed, ciphertext); err == nil {
		t.Fatal("modified request ID was accepted")
	}
	changed = header
	changed.StreamID = "other-stream"
	if _, err := agent.Open(changed, ciphertext); err == nil {
		t.Fatal("cross-stream ciphertext was accepted")
	}
	if _, err := client.Seal(header, []byte("nonce reuse")); err == nil {
		t.Fatal("duplicate send counter was accepted")
	}
}

func TestBlindHandshakeBindsCapabilityAndIdentity(t *testing.T) {
	binding := testBlindBinding(t)
	seed := bytes.Repeat([]byte{0x11}, ed25519.SeedSize)
	identity := ed25519.NewKeyFromSeed(seed)
	client, hello, err := NewBlindClientHandshake(binding, bytes.NewReader(bytes.Repeat([]byte{0x22}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	agent, response, err := AcceptBlindClientHello(binding, hello, identity, TrustBlind, bytes.NewReader(bytes.Repeat([]byte{0x33}, 32)))
	if err != nil {
		t.Fatal(err)
	}

	wrongBinding, err := NewStreamBinding(binding.DeviceID, binding.SessionID, binding.StreamID, binding.Subject, binding.Permission, []byte("different-capability"))
	if err != nil {
		t.Fatal(err)
	}
	wrongClient, _, err := NewBlindClientHandshake(wrongBinding, bytes.NewReader(bytes.Repeat([]byte{0x22}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := wrongClient.Finish(response, identity.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("response was not bound to capability")
	}

	otherIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x44}, ed25519.SeedSize))
	if _, _, err := client.Finish(response, otherIdentity.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("response signed by an untrusted identity was accepted")
	}
	// Client handshakes are consumed even on failure.
	if _, _, err := client.Finish(response, identity.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("client handshake reuse was accepted")
	}

	client2, hello2, err := NewBlindClientHandshake(binding, bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	agent2, response2, err := AcceptBlindClientHello(binding, hello2, identity, TrustBlind, bytes.NewReader(bytes.Repeat([]byte{0x66}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	clientCipher, finish, err := client2.Finish(response2, identity.Public().(ed25519.PublicKey))
	if err != nil || clientCipher == nil {
		t.Fatal(err)
	}
	finish.ClientConfirmation[0] ^= 1
	if _, err := agent2.Confirm(finish); err == nil {
		t.Fatal("invalid client key confirmation was accepted")
	}
	if _, err := agent2.Confirm(BlindClientFinish{}); err == nil {
		t.Fatal("agent handshake reuse was accepted")
	}
	_ = agent
}

func TestBlindTrustModeNegotiationAndDowngrade(t *testing.T) {
	mode, err := NegotiateTrustMode(TrustTrusted, []TrustMode{TrustTrusted, TrustBlind}, []TrustMode{TrustBlind, TrustTrusted}, false)
	if err != nil || mode != TrustBlind {
		t.Fatalf("strongest negotiation = %q, %v", mode, err)
	}
	_, err = NegotiateTrustMode(TrustBlind, []TrustMode{TrustTrusted, TrustBlind}, []TrustMode{TrustTrusted}, false)
	if !errors.Is(err, ErrTrustDowngrade) {
		t.Fatalf("downgrade error = %v", err)
	}
	mode, err = NegotiateTrustMode(TrustBlind, []TrustMode{TrustTrusted, TrustBlind}, []TrustMode{TrustTrusted}, true)
	if err != nil || mode != TrustTrusted {
		t.Fatalf("explicit downgrade = %q, %v", mode, err)
	}
	if err := EnforceTrustMode(TrustBlind, TrustTrusted, false); !errors.Is(err, ErrTrustDowngrade) {
		t.Fatalf("selected-mode downgrade error = %v", err)
	}
	if _, err := NegotiateTrustMode(TrustTrusted, []TrustMode{"future"}, []TrustMode{TrustTrusted}, false); err == nil {
		t.Fatal("unknown trust mode was accepted")
	}
}

func TestBlindKeyRotationAndFreshStreamRecovery(t *testing.T) {
	binding := testBlindBinding(t)
	client, agent, _, _, _ := deterministicBlindPair(t, binding)
	rotation, nextClient, err := client.NewRotation(bytes.NewReader(bytes.Repeat([]byte{0x77}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	nextAgent, err := agent.ApplyRotation(rotation)
	if err != nil {
		t.Fatal(err)
	}
	if nextClient.Epoch() != 2 || nextAgent.Epoch() != 2 {
		t.Fatalf("rotated epochs = %d, %d", nextClient.Epoch(), nextAgent.Epoch())
	}
	header := Header{Version: Version, FrameType: FrameSession, DeviceID: binding.DeviceID, SessionID: binding.SessionID, StreamID: binding.StreamID, Counter: 1}
	ciphertext, err := nextClient.Seal(header, []byte("after rotation"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := nextAgent.Open(header, ciphertext); err != nil || string(got) != "after rotation" {
		t.Fatalf("rotated open = %q, %v", got, err)
	}
	if _, err := agent.ApplyRotation(rotation); err == nil {
		t.Fatal("replayed rotation was accepted by the old epoch")
	}
	tampered := rotation
	tampered.Salt[0] ^= 1
	_, otherAgent, _, _, _ := deterministicBlindPair(t, binding)
	if _, err := otherAgent.ApplyRotation(tampered); err == nil {
		t.Fatal("forged rotation was accepted")
	}
	if _, err := nextAgent.ApplyRotation(rotation); err == nil {
		t.Fatal("stale rotation was accepted in new epoch")
	}

	// Recovery uses a fresh capability and stream, yielding unrelated keys and
	// counters even if an endpoint missed a rotation acknowledgement.
	recovered, err := NewRecoveryBinding(binding, "stream-recovered", []byte("fresh-single-use-capability"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecoveryBinding(binding, binding.StreamID, []byte("fresh-single-use-capability")); err == nil {
		t.Fatal("recovery reused the old stream ID")
	}
	if _, err := NewRecoveryBinding(binding, "another-stream", []byte("signed-capability-token")); err == nil {
		t.Fatal("recovery reused the old capability")
	}
	recoveryClient, recoveryAgent, _, _, _ := deterministicBlindPair(t, recovered)
	recoveryHeader := header
	recoveryHeader.StreamID = recovered.StreamID
	recoveryCiphertext, err := recoveryClient.Seal(recoveryHeader, []byte("fresh checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := recoveryAgent.Open(recoveryHeader, recoveryCiphertext); err != nil || string(got) != "fresh checkpoint" {
		t.Fatalf("recovery open = %q, %v", got, err)
	}
	if _, err := nextAgent.Open(recoveryHeader, recoveryCiphertext); err == nil {
		t.Fatal("recovery ciphertext opened on prior stream")
	}
}

func TestBlindGoldenVector(t *testing.T) {
	binding := testBlindBinding(t)
	client, _, hello, response, finish := deterministicBlindPair(t, binding)
	header := Header{Version: Version, FrameType: FrameSession, RequestID: 0x0102030405060708, DeviceID: binding.DeviceID, SessionID: binding.SessionID, StreamID: binding.StreamID, Counter: 1}
	ciphertext, err := client.Seal(header, []byte("golden terminal payload"))
	if err != nil {
		t.Fatal(err)
	}
	vectors := map[string]struct {
		got  []byte
		want string
	}{
		"client public":       {hello.EphemeralPublic, "132c442be010fbd57e72603328aa76e71fccc1503aae219327d14d9c9993f472"},
		"agent public":        {response.EphemeralPublic, "04bcd2e0d00f2cce5fe8f1c6c2fbec5c07fa56e3aa5c88a5689975d88b3fce05"},
		"agent signature":     {response.Signature, "2409f428e4baf17151cb3fa60b2a6e90da6dc2e271db4fd483c0e2ff4f8461084c47008cba7edf19970848f612d30baa986d2971fbbdac1c95204bd32fc6fd08"},
		"agent confirmation":  {response.AgentConfirmation, "497a9aea2d74afa81d636b8296cbbc8360131c9f27c4be79497e5d1e27b6f996"},
		"client confirmation": {finish.ClientConfirmation, "97a8fa217c922a05c333680082b53be3198fc6cf4d5b314abee5aefb7d16a4fc"},
		"ciphertext":          {ciphertext, "5cb3c5771dc5af63532e2773f9c9b2ae7fb94fd122fce3ff5a0b8cf18fdfa8d8868689debadf81"},
	}
	for name, vector := range vectors {
		if got := hex.EncodeToString(vector.got); got != vector.want {
			t.Errorf("%s = %s, want %s", name, got, vector.want)
		}
	}
}
