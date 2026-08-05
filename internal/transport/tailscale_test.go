package transport

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type runnerResponse struct {
	output []byte
	err    error
}

type recordingRunner struct {
	responses []runnerResponse
	calls     [][]string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(r.responses) == 0 {
		return nil, errors.New("unexpected command")
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.output, response.err
}

func TestParseTailscaleStatusOnlinePeers(t *testing.T) {
	peers, err := ParseTailscaleStatus([]byte(`{
        "BackendState":"Running",
        "Peer":{
          "nodekey:online":{"ID":"n1","StableID":"stable-1","HostName":"box","DNSName":"box.example.ts.net.","TailscaleIPs":["100.64.0.2"],"Online":true,"Active":true},
          "nodekey:offline":{"HostName":"old-box","TailscaleIPs":["100.64.0.3"],"Online":false}
        }
      }`))
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("peers = %+v", peers)
	}
	peer := peers[0]
	if peer.Key != "nodekey:online" || peer.StableID != "stable-1" || peer.DNSName != "box.example.ts.net" || !peer.Active {
		t.Fatalf("peer = %+v", peer)
	}
}

func TestParseTailscaleStatusRequiresRunningBackend(t *testing.T) {
	_, err := ParseTailscaleStatus([]byte(`{"BackendState":"NeedsLogin","Peer":{}}`))
	if err == nil || !strings.Contains(err.Error(), "NeedsLogin") {
		t.Fatalf("error = %v", err)
	}
}

func TestTailscaleResolverDiscoversAndChecksPeer(t *testing.T) {
	runner := &recordingRunner{responses: []runnerResponse{
		{output: []byte(`{"BackendState":"Running","Peer":{"nodekey:k":{"ID":"id-1","HostName":"agent","DNSName":"agent.tail.example.","TailscaleIPs":["100.64.0.8"],"Online":true}}}`)},
		{output: []byte("pong from agent (100.64.0.8) via TSMP in 1ms")},
	}}
	resolver := TailscaleResolver{
		Runner: runner, Peer: "id-1", Port: 9443, Path: "/shenmux/ws",
		PingTimeout: 1500 * time.Millisecond,
	}
	endpoint, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "ws://agent.tail.example:9443/shenmux/ws"
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
	wantCalls := [][]string{
		{"tailscale", "status", "--json"},
		{"tailscale", "ping", "--timeout=1.5s", "--tsmp", "--c=1", "agent.tail.example"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestTailscaleResolverRequiresActualDirectPath(t *testing.T) {
	runner := &recordingRunner{responses: []runnerResponse{
		{output: []byte(`{"BackendState":"Running","Peer":{"nodekey:k":{"HostName":"agent","TailscaleIPs":["100.64.0.8"],"Online":true}}}`)},
		{output: []byte("pong from agent (100.64.0.8) via DERP(ord) in 12ms")},
	}}
	_, err := (TailscaleResolver{Runner: runner, Peer: "agent", RequireDirect: true}).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no direct path") {
		t.Fatalf("error = %v", err)
	}
	if got := runner.calls[1]; !reflect.DeepEqual(got, []string{"tailscale", "ping", "--timeout=3s", "--until-direct=true", "100.64.0.8"}) {
		t.Fatalf("ping call = %#v", got)
	}
}

func TestTailscaleResolverIPv6Endpoint(t *testing.T) {
	runner := &recordingRunner{responses: []runnerResponse{{output: []byte(`{"BackendState":"Running","Peer":{"nodekey:k":{"HostName":"v6","TailscaleIPs":["fd7a:115c:a1e0::8"],"Online":true}}}`)}}}
	endpoint, err := (TailscaleResolver{Runner: runner, Peer: "v6", Scheme: "ws", Port: 8080, SkipPing: true}).Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := endpoint, "ws://[fd7a:115c:a1e0::8]:8080/ws"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}
