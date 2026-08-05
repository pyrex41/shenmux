// Package transport discovers optional direct paths used by the authenticated
// relay tunnel. The tunnel remains responsible for dialing, authentication,
// fallback, and reconnect policy.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CommandRunner is the small process boundary used by TailscaleResolver. It is
// injectable so discovery and health behavior do not require a live tailnet in
// tests.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// ExecRunner executes commands on the host.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// TailscalePeer contains only the stable status fields needed to select a
// peer. The tailscale status JSON format intentionally contains many more
// fields and may add fields without affecting this parser.
type TailscalePeer struct {
	Key          string
	ID           string
	StableID     string
	PublicKey    string
	HostName     string
	DNSName      string
	TailscaleIPs []string
	Online       bool
	Active       bool
}

type tailscaleStatus struct {
	BackendState string                     `json:"BackendState"`
	Peer         map[string]json.RawMessage `json:"Peer"`
}

type tailscalePeerJSON struct {
	ID           string   `json:"ID"`
	StableID     string   `json:"StableID"`
	PublicKey    string   `json:"PublicKey"`
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	Active       bool     `json:"Active"`
}

// ParseTailscaleStatus returns online peers from tailscale status --json.
// Offline peers are deliberately excluded so a stale tailnet record never
// displaces the always-available relay fallback.
func ParseTailscaleStatus(data []byte) ([]TailscalePeer, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var status tailscaleStatus
	if err := decoder.Decode(&status); err != nil {
		return nil, fmt.Errorf("decode tailscale status: %w", err)
	}
	if status.BackendState != "" && !strings.EqualFold(status.BackendState, "running") {
		return nil, fmt.Errorf("tailscale backend is %s", status.BackendState)
	}
	peers := make([]TailscalePeer, 0, len(status.Peer))
	for key, raw := range status.Peer {
		var value tailscalePeerJSON
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("decode tailscale peer %q: %w", key, err)
		}
		if !value.Online {
			continue
		}
		peer := TailscalePeer{
			Key: key, ID: value.ID, StableID: value.StableID,
			PublicKey: value.PublicKey, HostName: value.HostName,
			DNSName:      strings.TrimSuffix(value.DNSName, "."),
			TailscaleIPs: append([]string(nil), value.TailscaleIPs...),
			Online:       value.Online, Active: value.Active,
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

// TailscaleResolver discovers an online peer and exposes the shenmux WSS
// service over its WireGuard-protected tailnet address. Peer may be a status
// map key, node/stable ID, public key, hostname, MagicDNS name, or Tailscale IP.
type TailscaleResolver struct {
	Runner        CommandRunner
	Binary        string
	Peer          string
	Scheme        string
	Port          int
	Path          string
	PingTimeout   time.Duration
	SkipPing      bool
	RequireDirect bool
}

// Resolve returns the WebSocket URL for an online peer after checking tailnet
// reachability. The caller decides whether failure is fatal or should fall
// back to the relay.
func (r TailscaleResolver) Resolve(ctx context.Context) (string, error) {
	runner := r.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	binary := r.Binary
	if binary == "" {
		binary = "tailscale"
	}
	output, err := runner.Run(ctx, binary, "status", "--json")
	if err != nil {
		return "", commandError("tailscale status", output, err)
	}
	peers, err := ParseTailscaleStatus(output)
	if err != nil {
		return "", err
	}
	peer, err := selectTailscalePeer(peers, r.Peer)
	if err != nil {
		return "", err
	}
	target := peerTarget(peer)
	if target == "" {
		return "", fmt.Errorf("tailscale peer %q has no DNS name or IP", r.Peer)
	}
	if !r.SkipPing {
		if err := r.ping(ctx, runner, binary, target); err != nil {
			return "", err
		}
	}
	// The peer listener is HTTP by default because the tailnet already
	// authenticates and encrypts this hop with WireGuard. Callers can request
	// wss explicitly when the peer uses Tailscale HTTPS certificates/Serve.
	endpointConfig := r
	if endpointConfig.Scheme == "" {
		endpointConfig.Scheme = "ws"
	}
	endpointURL, err := endpointConfig.endpointURL(target)
	if err != nil {
		return "", err
	}
	return endpointURL, nil
}

func (r TailscaleResolver) ping(ctx context.Context, runner CommandRunner, binary, target string) error {
	timeout := r.PingTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	args := []string{"ping", "--timeout=" + timeout.String()}
	if r.RequireDirect {
		args = append(args, "--until-direct=true")
	} else {
		// TSMP verifies end-to-end tailnet reachability without depending on
		// the peer operating system accepting ICMP.
		args = append(args, "--tsmp", "--c=1")
	}
	args = append(args, target)
	output, err := runner.Run(ctx, binary, args...)
	if err != nil {
		return commandError("tailscale ping", output, err)
	}
	if r.RequireDirect && !pingReportsDirect(output) {
		return errors.New("tailscale peer is reachable but no direct path was established")
	}
	return nil
}

func (r TailscaleResolver) endpointURL(target string) (string, error) {
	scheme := r.Scheme
	if scheme == "" {
		scheme = "wss"
	}
	if scheme != "ws" && scheme != "wss" {
		return "", fmt.Errorf("unsupported tailscale websocket scheme %q", scheme)
	}
	port := r.Port
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("invalid tailscale websocket port %d", port)
	}
	host := target
	if port != 0 {
		host = net.JoinHostPort(target, strconv.Itoa(port))
	} else if strings.Contains(target, ":") {
		host = "[" + target + "]"
	}
	path := r.Path
	if path == "" {
		path = "/ws"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: scheme, Host: host, Path: path}
	return u.String(), nil
}

func selectTailscalePeer(peers []TailscalePeer, query string) (TailscalePeer, error) {
	if query == "" {
		if len(peers) == 1 {
			return peers[0], nil
		}
		return TailscalePeer{}, fmt.Errorf("tailscale peer is required (%d online peers)", len(peers))
	}
	normalized := strings.TrimSuffix(strings.ToLower(query), ".")
	var matches []TailscalePeer
	for _, peer := range peers {
		values := []string{peer.Key, peer.ID, peer.StableID, peer.PublicKey, peer.HostName, peer.DNSName}
		values = append(values, peer.TailscaleIPs...)
		for _, value := range values {
			if strings.TrimSuffix(strings.ToLower(value), ".") == normalized {
				matches = append(matches, peer)
				break
			}
		}
	}
	if len(matches) == 0 {
		return TailscalePeer{}, fmt.Errorf("online tailscale peer %q not found", query)
	}
	if len(matches) > 1 {
		return TailscalePeer{}, fmt.Errorf("tailscale peer %q is ambiguous", query)
	}
	return matches[0], nil
}

func peerTarget(peer TailscalePeer) string {
	if peer.DNSName != "" {
		return peer.DNSName
	}
	for _, address := range peer.TailscaleIPs {
		if net.ParseIP(address) != nil {
			return address
		}
	}
	return ""
}

func pingReportsDirect(output []byte) bool {
	text := strings.ToLower(string(output))
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "pong from") || !strings.Contains(line, " via ") {
			continue
		}
		if !strings.Contains(line, "via derp(") && !strings.Contains(line, "via peer-relay(") {
			return true
		}
	}
	return false
}

func commandError(operation string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, detail)
}
