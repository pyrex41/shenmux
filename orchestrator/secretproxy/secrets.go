package secretproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Secret is one credential the proxy knows how to inject. For the demo the real
// Value is inline; in production Value is omitted and BaoRef names an OpenBao
// secret to resolve at load time. BaoRef is documented/carried but unused in
// demo mode (see CONTRACTS.md §1).
type Secret struct {
	Placeholder  string   `json:"placeholder"`
	Value        string   `json:"value"`
	BaoRef       string   `json:"bao_ref,omitempty"`
	AllowedHosts []string `json:"allowed_hosts"`
}

// Secrets is the parsed secrets.json file.
type Secrets struct {
	Secrets map[string]Secret `json:"secrets"`
}

// LoadSecrets reads and parses a secrets.json file.
func LoadSecrets(path string) (*Secrets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read secrets file: %w", err)
	}
	var s Secrets
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("cannot parse secrets file: %w", err)
	}
	return &s, nil
}

// hostMatches reports whether host is covered by an allowed_hosts pattern.
//
// This is the deliberate divergence from the reference implementation, which
// only did exact matching. Per CONTRACTS.md §1 we support suffix matching:
//
//   - exact match: "api.example.com" matches "api.example.com"
//   - suffix match: ".example.com" matches "api.example.com" (and, for
//     convenience, the bare "example.com" too)
//
// Matching is case-insensitive and tolerant of a trailing dot on the host
// (a fully-qualified "api.example.com." is treated as "api.example.com").
func hostMatches(pattern, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	pattern = strings.ToLower(pattern)

	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, ".") {
		// ".example.com" matches any sub-domain "…​.example.com".
		if strings.HasSuffix(host, pattern) {
			return true
		}
		// ".example.com" also matches the apex "example.com".
		if host == strings.TrimPrefix(pattern, ".") {
			return true
		}
	}
	return false
}

// secretAllowsHost reports whether this secret may be injected for host.
func secretAllowsHost(s Secret, host string) bool {
	for _, p := range s.AllowedHosts {
		if hostMatches(p, host) {
			return true
		}
	}
	return false
}

// hostAllowed reports whether ANY configured secret allows host. Used to decide
// whether a CONNECT tunnel is MITM'd (some secret may apply) or blind-tunnelled.
func (s *Secrets) hostAllowed(host string) bool {
	for _, sec := range s.Secrets {
		if secretAllowsHost(sec, host) {
			return true
		}
	}
	return false
}
