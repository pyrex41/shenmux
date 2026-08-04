package relay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

const (
	// BlindVersion is the version of the end-to-end stream encryption
	// handshake. It is independent of the relay envelope and terminal protocol
	// versions so each layer can evolve without silently changing another.
	BlindVersion uint16 = 1

	blindTranscriptDomain = "shenmux-blind-stream-v1"
	blindKDFDomain        = "shenmux-blind-stream-keys-v1"
	blindRotationDomain   = "shenmux-blind-stream-rotation-v1"
	blindNonceSize        = 12
	blindKeySize          = 32
)

// TrustMode is an explicit transport-content trust decision. TLS is required
// in both modes; Blind additionally encrypts the inner terminal frames between
// the client and agent.
type TrustMode string

const (
	TrustTrusted TrustMode = "trusted"
	TrustBlind   TrustMode = "blind"
)

var ErrTrustDowngrade = errors.New("relay trust mode downgrade rejected")

func (m TrustMode) Validate() error {
	switch m {
	case TrustTrusted, TrustBlind:
		return nil
	default:
		return fmt.Errorf("unknown relay trust mode %q", m)
	}
}

func trustRank(m TrustMode) int {
	if m == TrustBlind {
		return 1
	}
	return 0
}

// NegotiateTrustMode chooses the strongest mutually supported mode and then
// enforces minimum. Falling below a persisted blind minimum is allowed only
// when allowDowngrade was explicitly set by the user for this negotiation.
func NegotiateTrustMode(minimum TrustMode, local, peer []TrustMode, allowDowngrade bool) (TrustMode, error) {
	if err := minimum.Validate(); err != nil {
		return "", err
	}
	for _, modes := range [][]TrustMode{local, peer} {
		if len(modes) == 0 {
			return "", errors.New("relay trust mode offer must not be empty")
		}
		for _, mode := range modes {
			if err := mode.Validate(); err != nil {
				return "", err
			}
		}
	}
	for _, candidate := range []TrustMode{TrustBlind, TrustTrusted} {
		if !slices.Contains(local, candidate) || !slices.Contains(peer, candidate) {
			continue
		}
		if trustRank(candidate) < trustRank(minimum) && !allowDowngrade {
			return "", fmt.Errorf("%w: minimum %s, peer negotiated %s", ErrTrustDowngrade, minimum, candidate)
		}
		return candidate, nil
	}
	return "", errors.New("no mutually supported relay trust mode")
}

// EnforceTrustMode is used when the peer selected a mode rather than sending a
// full offer. It prevents an on-path or controller downgrade from blind mode.
func EnforceTrustMode(minimum, negotiated TrustMode, allowDowngrade bool) error {
	if err := minimum.Validate(); err != nil {
		return err
	}
	if err := negotiated.Validate(); err != nil {
		return err
	}
	if trustRank(negotiated) < trustRank(minimum) && !allowDowngrade {
		return fmt.Errorf("%w: minimum %s, negotiated %s", ErrTrustDowngrade, minimum, negotiated)
	}
	return nil
}

// StreamBinding is the authorization and routing context authenticated by the
// key exchange. CapabilityDigest is SHA-256 over the exact opaque attach
// capability received from the controller. The capability is never used as
// key material and need not be disclosed to the relay as part of the
// handshake. Epoch starts at one and increases on rotation.
type StreamBinding struct {
	DeviceID         string
	SessionID        string
	StreamID         string
	Subject          string
	Permission       string
	CapabilityDigest [32]byte
	Epoch            uint64
}

func NewStreamBinding(deviceID, sessionID, streamID, subject, permission string, capability []byte) (StreamBinding, error) {
	b := StreamBinding{
		DeviceID: deviceID, SessionID: sessionID, StreamID: streamID,
		Subject: subject, Permission: permission,
		CapabilityDigest: sha256.Sum256(capability), Epoch: 1,
	}
	if len(capability) == 0 {
		return StreamBinding{}, errors.New("attach capability must not be empty")
	}
	if err := b.Validate(); err != nil {
		return StreamBinding{}, err
	}
	return b, nil
}

// NewRecoveryBinding creates the context for recovery after a lost key update
// or reconnect. Recovery always uses a new single-use capability, a new stream
// ID, epoch one, and a complete handshake. This keeps old ciphertext and
// counters cryptographically disjoint from the recovered stream.
func NewRecoveryBinding(previous StreamBinding, newStreamID string, capability []byte) (StreamBinding, error) {
	if err := previous.Validate(); err != nil {
		return StreamBinding{}, err
	}
	if newStreamID == previous.StreamID {
		return StreamBinding{}, errors.New("blind recovery requires a new stream ID")
	}
	next, err := NewStreamBinding(previous.DeviceID, previous.SessionID, newStreamID, previous.Subject, previous.Permission, capability)
	if err != nil {
		return StreamBinding{}, err
	}
	if hmac.Equal(next.CapabilityDigest[:], previous.CapabilityDigest[:]) {
		return StreamBinding{}, errors.New("blind recovery requires a fresh capability")
	}
	return next, nil
}

func (b StreamBinding) Validate() error {
	for name, value := range map[string]string{
		"device ID": b.DeviceID, "session ID": b.SessionID,
		"stream ID": b.StreamID, "subject": b.Subject, "permission": b.Permission,
	} {
		if value == "" {
			return fmt.Errorf("blind stream binding has no %s", name)
		}
	}
	if b.Epoch == 0 {
		return errors.New("blind stream epoch must be greater than zero")
	}
	var zero [32]byte
	if hmac.Equal(b.CapabilityDigest[:], zero[:]) {
		return errors.New("blind stream binding has no capability digest")
	}
	return nil
}

// BlindClientHello starts an authenticated ephemeral X25519 exchange. The
// binding is carried independently by the OPEN request and is included in the
// signed transcript by both endpoints.
type BlindClientHello struct {
	Version         uint16    `json:"version"`
	TrustMode       TrustMode `json:"trust_mode"`
	Epoch           uint64    `json:"epoch"`
	EphemeralPublic []byte    `json:"ephemeral_public"`
}

// BlindAgentResponse authenticates the agent ephemeral key with its enrolled
// Ed25519 identity and proves possession of the resulting X25519 secret.
type BlindAgentResponse struct {
	Version           uint16 `json:"version"`
	Epoch             uint64 `json:"epoch"`
	EphemeralPublic   []byte `json:"ephemeral_public"`
	Signature         []byte `json:"signature"`
	AgentConfirmation []byte `json:"agent_confirmation"`
}

// BlindClientFinish gives the agent explicit key confirmation before any
// terminal contents are accepted on the stream.
type BlindClientFinish struct {
	Version            uint16 `json:"version"`
	Epoch              uint64 `json:"epoch"`
	ClientConfirmation []byte `json:"client_confirmation"`
}

// BlindClientHandshake retains the ephemeral private key only until Finish.
// Handshake instances are single-use.
type BlindClientHandshake struct {
	mu      sync.Mutex
	binding StreamBinding
	private *ecdh.PrivateKey
	hello   BlindClientHello
	used    bool
}

// BlindAgentHandshake retains pending key material until client confirmation.
// Handshake instances are single-use.
type BlindAgentHandshake struct {
	mu           sync.Mutex
	binding      StreamBinding
	transcript   [32]byte
	signature    []byte
	confirmation [32]byte
	material     streamKeyMaterial
	used         bool
}

func NewBlindClientHandshake(binding StreamBinding, random io.Reader) (*BlindClientHandshake, BlindClientHello, error) {
	if err := binding.Validate(); err != nil {
		return nil, BlindClientHello{}, err
	}
	private, err := generateX25519(random)
	if err != nil {
		return nil, BlindClientHello{}, err
	}
	hello := BlindClientHello{
		Version: BlindVersion, TrustMode: TrustBlind, Epoch: binding.Epoch,
		EphemeralPublic: append([]byte(nil), private.PublicKey().Bytes()...),
	}
	return &BlindClientHandshake{binding: binding, private: private, hello: hello}, hello, nil
}

// AcceptBlindClientHello verifies the negotiated mode and creates an
// agent-authenticated response. It does not return usable stream keys until
// the caller has checked BlindClientFinish with Confirm.
func AcceptBlindClientHello(binding StreamBinding, hello BlindClientHello, identity ed25519.PrivateKey, minimum TrustMode, random io.Reader) (*BlindAgentHandshake, BlindAgentResponse, error) {
	if err := binding.Validate(); err != nil {
		return nil, BlindAgentResponse{}, err
	}
	if err := EnforceTrustMode(minimum, hello.TrustMode, false); err != nil {
		return nil, BlindAgentResponse{}, err
	}
	if hello.TrustMode != TrustBlind {
		return nil, BlindAgentResponse{}, errors.New("blind handshake requires blind trust mode")
	}
	if hello.Version != BlindVersion || hello.Epoch != binding.Epoch {
		return nil, BlindAgentResponse{}, errors.New("blind client hello version or epoch mismatch")
	}
	if len(identity) != ed25519.PrivateKeySize {
		return nil, BlindAgentResponse{}, errors.New("invalid agent Ed25519 private key")
	}
	clientPublic, err := ecdh.X25519().NewPublicKey(hello.EphemeralPublic)
	if err != nil {
		return nil, BlindAgentResponse{}, fmt.Errorf("invalid client X25519 public key: %w", err)
	}
	private, err := generateX25519(random)
	if err != nil {
		return nil, BlindAgentResponse{}, err
	}
	shared, err := private.ECDH(clientPublic)
	if err != nil {
		return nil, BlindAgentResponse{}, fmt.Errorf("X25519 client exchange: %w", err)
	}
	agentPublic := private.PublicKey().Bytes()
	transcript := blindTranscript(binding, hello.EphemeralPublic, agentPublic)
	signature := ed25519.Sign(identity, transcript[:])
	material, err := deriveStreamKeyMaterial(shared, transcript[:], binding.Epoch)
	if err != nil {
		return nil, BlindAgentResponse{}, err
	}
	agentConfirmation := confirm(material.confirmation[:], "agent", transcript[:], signature)
	clientConfirmation := confirm(material.confirmation[:], "client", transcript[:], signature)
	var expectedClientConfirmation [32]byte
	copy(expectedClientConfirmation[:], clientConfirmation)
	pending := &BlindAgentHandshake{
		binding: binding, transcript: transcript, signature: append([]byte(nil), signature...),
		confirmation: expectedClientConfirmation, material: material,
	}
	response := BlindAgentResponse{
		Version: BlindVersion, Epoch: binding.Epoch,
		EphemeralPublic: append([]byte(nil), agentPublic...), Signature: append([]byte(nil), signature...),
		AgentConfirmation: append([]byte(nil), agentConfirmation...),
	}
	return pending, response, nil
}

// Finish authenticates the agent response and returns the client cipher plus
// the confirmation that must be accepted by the agent. A failed attempt also
// consumes the ephemeral handshake to prevent ambiguous retries.
func (h *BlindClientHandshake) Finish(response BlindAgentResponse, identity ed25519.PublicKey) (*StreamCipher, BlindClientFinish, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.used {
		return nil, BlindClientFinish{}, errors.New("blind client handshake already used")
	}
	h.used = true
	if response.Version != BlindVersion || response.Epoch != h.binding.Epoch {
		return nil, BlindClientFinish{}, errors.New("blind agent response version or epoch mismatch")
	}
	if len(identity) != ed25519.PublicKeySize || len(response.Signature) != ed25519.SignatureSize {
		return nil, BlindClientFinish{}, errors.New("invalid agent Ed25519 proof")
	}
	agentPublic, err := ecdh.X25519().NewPublicKey(response.EphemeralPublic)
	if err != nil {
		return nil, BlindClientFinish{}, fmt.Errorf("invalid agent X25519 public key: %w", err)
	}
	transcript := blindTranscript(h.binding, h.hello.EphemeralPublic, response.EphemeralPublic)
	if !ed25519.Verify(identity, transcript[:], response.Signature) {
		return nil, BlindClientFinish{}, errors.New("invalid blind handshake agent signature")
	}
	shared, err := h.private.ECDH(agentPublic)
	if err != nil {
		return nil, BlindClientFinish{}, fmt.Errorf("X25519 agent exchange: %w", err)
	}
	material, err := deriveStreamKeyMaterial(shared, transcript[:], h.binding.Epoch)
	if err != nil {
		return nil, BlindClientFinish{}, err
	}
	wantAgent := confirm(material.confirmation[:], "agent", transcript[:], response.Signature)
	if !hmac.Equal(response.AgentConfirmation, wantAgent) {
		return nil, BlindClientFinish{}, errors.New("invalid blind handshake agent key confirmation")
	}
	clientConfirmation := confirm(material.confirmation[:], "client", transcript[:], response.Signature)
	cipher, err := newStreamCipher(h.binding, roleClient, material)
	if err != nil {
		return nil, BlindClientFinish{}, err
	}
	finish := BlindClientFinish{Version: BlindVersion, Epoch: h.binding.Epoch, ClientConfirmation: clientConfirmation}
	return cipher, finish, nil
}

// Confirm verifies client possession of the stream secret and returns the
// agent side of the directional cipher.
func (h *BlindAgentHandshake) Confirm(finish BlindClientFinish) (*StreamCipher, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.used {
		return nil, errors.New("blind agent handshake already used")
	}
	h.used = true
	if finish.Version != BlindVersion || finish.Epoch != h.binding.Epoch {
		return nil, errors.New("blind client finish version or epoch mismatch")
	}
	if !hmac.Equal(finish.ClientConfirmation, h.confirmation[:]) {
		return nil, errors.New("invalid blind handshake client key confirmation")
	}
	return newStreamCipher(h.binding, roleAgent, h.material)
}

type streamRole uint8

const (
	roleClient streamRole = iota + 1
	roleAgent
)

type streamKeyMaterial struct {
	root          [blindKeySize]byte
	clientToAgent [blindKeySize]byte
	agentToClient [blindKeySize]byte
	confirmation  [blindKeySize]byte
	nonceClient   [4]byte
	nonceAgent    [4]byte
}

// StreamCipher encrypts the complete inner protocol payload. Its AEAD
// additional data authenticates every outer routing field, the stream binding,
// direction, and key epoch. Counters are strictly increasing in each
// direction, which rejects replay and nonce reuse.
type StreamCipher struct {
	mu           sync.Mutex
	binding      StreamBinding
	role         streamRole
	material     streamKeyMaterial
	send         cipher.AEAD
	receive      cipher.AEAD
	sendBase     [4]byte
	recvBase     [4]byte
	sendLast     uint64
	recvLast     uint64
	rotationUsed bool
}

func newStreamCipher(binding StreamBinding, role streamRole, material streamKeyMaterial) (*StreamCipher, error) {
	sendKey, receiveKey := material.clientToAgent[:], material.agentToClient[:]
	sendBase, receiveBase := material.nonceClient, material.nonceAgent
	if role == roleAgent {
		sendKey, receiveKey = material.agentToClient[:], material.clientToAgent[:]
		sendBase, receiveBase = material.nonceAgent, material.nonceClient
	}
	seal, err := newGCM(sendKey)
	if err != nil {
		return nil, err
	}
	open, err := newGCM(receiveKey)
	if err != nil {
		return nil, err
	}
	return &StreamCipher{binding: binding, role: role, material: material, send: seal, receive: open, sendBase: sendBase, recvBase: receiveBase}, nil
}

// Seal encrypts payload for header. Counter zero and repeated/rolled-back
// counters are rejected before encryption to guarantee unique AEAD nonces.
func (s *StreamCipher) Seal(header Header, payload []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateHeader(header); err != nil {
		return nil, err
	}
	if header.Counter == 0 || header.Counter <= s.sendLast {
		return nil, fmt.Errorf("blind send counter %d is not greater than %d", header.Counter, s.sendLast)
	}
	nonce := blindNonce(s.sendBase, header.Counter)
	aad := blindAAD(s.binding, s.sendDirection(), header)
	ciphertext := s.send.Seal(nil, nonce[:], payload, aad)
	s.sendLast = header.Counter
	return ciphertext, nil
}

// Open authenticates and decrypts payload. The replay counter advances only
// after successful authentication, so corrupted packets cannot consume a
// valid future counter.
func (s *StreamCipher) Open(header Header, ciphertext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateHeader(header); err != nil {
		return nil, err
	}
	if header.Counter == 0 || header.Counter <= s.recvLast {
		return nil, fmt.Errorf("blind receive counter %d is not greater than %d", header.Counter, s.recvLast)
	}
	nonce := blindNonce(s.recvBase, header.Counter)
	aad := blindAAD(s.binding, s.receiveDirection(), header)
	plaintext, err := s.receive.Open(nil, nonce[:], ciphertext, aad)
	if err != nil {
		return nil, errors.New("blind payload authentication failed")
	}
	s.recvLast = header.Counter
	return plaintext, nil
}

// SealEnvelope preserves the outer routing header and encrypts only its inner
// payload, leaving the controller with the minimum metadata needed to route.
func (s *StreamCipher) SealEnvelope(env Envelope) (Envelope, error) {
	payload, err := s.Seal(env.Header, env.Payload)
	if err != nil {
		return Envelope{}, err
	}
	env.Payload = payload
	return env, nil
}

func (s *StreamCipher) OpenEnvelope(env Envelope) (Envelope, error) {
	payload, err := s.Open(env.Header, env.Payload)
	if err != nil {
		return Envelope{}, err
	}
	env.Payload = payload
	return env, nil
}

func (s *StreamCipher) Epoch() uint64 { return s.binding.Epoch }

func (s *StreamCipher) validateHeader(h Header) error {
	if h.Version != 0 && h.Version != Version {
		return fmt.Errorf("blind envelope has unsupported relay version %d", h.Version)
	}
	if h.DeviceID != s.binding.DeviceID || h.SessionID != s.binding.SessionID || h.StreamID != s.binding.StreamID {
		return errors.New("blind envelope routing does not match stream binding")
	}
	if h.FrameType == "" {
		return errors.New("blind envelope frame type must not be empty")
	}
	return nil
}

func (s *StreamCipher) sendDirection() string {
	if s.role == roleClient {
		return "client-to-agent"
	}
	return "agent-to-client"
}

func (s *StreamCipher) receiveDirection() string {
	if s.role == roleClient {
		return "agent-to-client"
	}
	return "client-to-agent"
}

// BlindKeyRotation is sent through the already encrypted stream before either
// endpoint switches keys. ApplyRotation returns a new cipher with reset
// counters; callers atomically replace the old cipher only after the update is
// acknowledged. A missed or ambiguous update is recovered with a fresh stream
// and capability-bound handshake, never by reusing nonces or falling back to
// trusted mode.
type BlindKeyRotation struct {
	Version       uint16   `json:"version"`
	PreviousEpoch uint64   `json:"previous_epoch"`
	Epoch         uint64   `json:"epoch"`
	Salt          [32]byte `json:"salt"`
	Confirmation  [32]byte `json:"confirmation"`
}

func (s *StreamCipher) NewRotation(random io.Reader) (BlindKeyRotation, *StreamCipher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotationUsed {
		return BlindKeyRotation{}, nil, errors.New("blind key rotation already created or applied")
	}
	if random == nil {
		random = rand.Reader
	}
	rotation := BlindKeyRotation{Version: BlindVersion, PreviousEpoch: s.binding.Epoch, Epoch: s.binding.Epoch + 1}
	if rotation.Epoch == 0 {
		return BlindKeyRotation{}, nil, errors.New("blind key epoch overflow")
	}
	if _, err := io.ReadFull(random, rotation.Salt[:]); err != nil {
		return BlindKeyRotation{}, nil, fmt.Errorf("generate blind rotation salt: %w", err)
	}
	rotation.Confirmation = rotationConfirmation(s.material.root[:], s.binding, rotation)
	next, err := s.rotated(rotation)
	if err == nil {
		s.rotationUsed = true
	}
	return rotation, next, err
}

func (s *StreamCipher) ApplyRotation(rotation BlindKeyRotation) (*StreamCipher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotationUsed {
		return nil, errors.New("blind key rotation already created or applied")
	}
	if rotation.Version != BlindVersion || rotation.PreviousEpoch != s.binding.Epoch || rotation.Epoch != s.binding.Epoch+1 || rotation.Epoch == 0 {
		return nil, errors.New("blind key rotation epoch mismatch")
	}
	want := rotationConfirmation(s.material.root[:], s.binding, rotation)
	if !hmac.Equal(rotation.Confirmation[:], want[:]) {
		return nil, errors.New("invalid blind key rotation confirmation")
	}
	next, err := s.rotated(rotation)
	if err == nil {
		s.rotationUsed = true
	}
	return next, err
}

func (s *StreamCipher) rotated(rotation BlindKeyRotation) (*StreamCipher, error) {
	nextRoot := hkdfSHA256(s.material.root[:], rotation.Salt[:], []byte(blindRotationDomain), blindKeySize)
	nextBinding := s.binding
	nextBinding.Epoch = rotation.Epoch
	context := bindingBytes(nextBinding)
	material, err := expandStreamKeyMaterial(nextRoot, context, nextBinding.Epoch)
	if err != nil {
		return nil, err
	}
	return newStreamCipher(nextBinding, s.role, material)
}

func generateX25519(random io.Reader) (*ecdh.PrivateKey, error) {
	if random == nil {
		random = rand.Reader
	}
	// Read the scalar explicitly instead of ecdh.GenerateKey so deterministic
	// protocol vectors do not depend on crypto/internal/randutil's optional
	// extra read. X25519 applies the required scalar clamping internally.
	scalar := make([]byte, 32)
	if _, err := io.ReadFull(random, scalar); err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	private, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	return private, nil
}

func blindTranscript(binding StreamBinding, clientPublic, agentPublic []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(blindTranscriptDomain))
	writeUint16(h, BlindVersion)
	writeField(h, []byte(TrustBlind))
	h.Write(bindingBytes(binding))
	writeField(h, clientPublic)
	writeField(h, agentPublic)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func bindingBytes(binding StreamBinding) []byte {
	var out bytes.Buffer
	writeField(&out, []byte(binding.DeviceID))
	writeField(&out, []byte(binding.SessionID))
	writeField(&out, []byte(binding.StreamID))
	writeField(&out, []byte(binding.Subject))
	writeField(&out, []byte(binding.Permission))
	writeField(&out, binding.CapabilityDigest[:])
	writeUint64(&out, binding.Epoch)
	return out.Bytes()
}

func deriveStreamKeyMaterial(shared, transcript []byte, epoch uint64) (streamKeyMaterial, error) {
	root := hkdfSHA256(shared, transcript, []byte(blindKDFDomain+"/root"), blindKeySize)
	return expandStreamKeyMaterial(root, transcript, epoch)
}

func expandStreamKeyMaterial(root, context []byte, epoch uint64) (streamKeyMaterial, error) {
	if len(root) != blindKeySize {
		return streamKeyMaterial{}, errors.New("invalid blind root key size")
	}
	info := append([]byte(nil), context...)
	var epochBytes [8]byte
	binary.BigEndian.PutUint64(epochBytes[:], epoch)
	info = append(info, epochBytes[:]...)
	expanded := hkdfSHA256(root, nil, append([]byte(blindKDFDomain+"/expand/"), info...), 32*4+8)
	var material streamKeyMaterial
	copy(material.root[:], expanded[:32])
	copy(material.clientToAgent[:], expanded[32:64])
	copy(material.agentToClient[:], expanded[64:96])
	copy(material.confirmation[:], expanded[96:128])
	copy(material.nonceClient[:], expanded[128:132])
	copy(material.nonceAgent[:], expanded[132:136])
	return material, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create blind AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create blind GCM: %w", err)
	}
	return aead, nil
}

func blindNonce(base [4]byte, counter uint64) [blindNonceSize]byte {
	var nonce [blindNonceSize]byte
	copy(nonce[:4], base[:])
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func blindAAD(binding StreamBinding, direction string, header Header) []byte {
	var out bytes.Buffer
	out.WriteString(blindKDFDomain + "/aad")
	out.Write(bindingBytes(binding))
	writeField(&out, []byte(direction))
	writeUint16(&out, Version)
	writeField(&out, []byte(header.FrameType))
	writeUint64(&out, header.RequestID)
	writeField(&out, []byte(header.DeviceID))
	writeField(&out, []byte(header.SessionID))
	writeField(&out, []byte(header.StreamID))
	writeUint64(&out, header.Counter)
	return out.Bytes()
}

func confirm(key []byte, party string, transcript, signature []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(blindKDFDomain + "/confirm/" + party))
	writeField(mac, transcript)
	writeField(mac, signature)
	return mac.Sum(nil)
}

func rotationConfirmation(root []byte, binding StreamBinding, rotation BlindKeyRotation) [32]byte {
	mac := hmac.New(sha256.New, root)
	mac.Write([]byte(blindRotationDomain))
	mac.Write(bindingBytes(binding))
	writeUint16(mac, rotation.Version)
	writeUint64(mac, rotation.PreviousEpoch)
	writeUint64(mac, rotation.Epoch)
	mac.Write(rotation.Salt[:])
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// hkdfSHA256 is RFC 5869 extract-and-expand. Keeping this tiny implementation
// local avoids adding a dependency solely for HKDF on Go versions before the
// standard-library crypto/hkdf package.
func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	if salt == nil {
		salt = make([]byte, sha256.Size)
	}
	extract := hmac.New(sha256.New, salt)
	extract.Write(secret)
	prk := extract.Sum(nil)
	out := make([]byte, 0, length)
	var previous []byte
	for counter := byte(1); len(out) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		expand.Write(previous)
		expand.Write(info)
		expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		remaining := length - len(out)
		if remaining > len(previous) {
			remaining = len(previous)
		}
		out = append(out, previous[:remaining]...)
	}
	return out
}

func writeField(w io.Writer, value []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(value)
}

func writeUint16(w io.Writer, value uint16) {
	var data [2]byte
	binary.BigEndian.PutUint16(data[:], value)
	_, _ = w.Write(data[:])
}

func writeUint64(w io.Writer, value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	_, _ = w.Write(data[:])
}
