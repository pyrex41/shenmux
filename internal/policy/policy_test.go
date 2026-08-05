package policy

import (
	"path/filepath"
	"testing"
	"time"
)

func TestACLAndCapabilityPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "policy.json")
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.AddGrant("alice", "device-1", "sess", []Permission{PermissionObserve, PermissionControl}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cap, err := s.IssueCapability("alice", "device-1", "sess", PermissionControl, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateCapability(cap, PermissionControl, time.Now()); err != nil {
		t.Fatal(err)
	}
	s2, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Grant(grant.ID); !ok {
		t.Fatal("grant not persisted")
	}
	if err := s2.ValidateCapability(cap, PermissionControl, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s2.RevokeGrant(grant.ID, "access removed"); err != nil {
		t.Fatal(err)
	}
	if err := s2.ValidateCapability(cap, PermissionControl, time.Now()); err == nil {
		t.Fatal("capability remained valid after grant revocation")
	}
	if _, err := s2.IssueCapability("bob", "device-1", "sess", PermissionObserve, time.Minute); err == nil {
		t.Fatal("unauthorized issue succeeded")
	}
}

func TestRevocationAndLeaseExpiry(t *testing.T) {
	s := NewMemory()
	if _, err := s.AddGrant("alice", "d", "s", []Permission{PermissionControl}, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cap, err := s.IssueCapability("alice", "d", "s", PermissionControl, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireLease(cap, time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(cap, time.Second, now); err != nil {
		t.Fatalf("same owner should renew/replace lease: %v", err)
	}
	if got := s.ReapLeases(now.Add(2 * time.Second)); len(got) != 1 || got[0].Session != lease.Session {
		t.Fatalf("unexpected expired leases: %#v", got)
	}
	if err := s.RevokeDevice("d", "compromised"); err != nil {
		t.Fatal(err)
	}
	if !s.IsDeviceRevoked("d") {
		t.Fatal("device revocation not visible")
	}
	if err := s.ValidateCapability(cap, PermissionControl, now); err == nil {
		t.Fatal("revoked device capability remained valid")
	}
	if revs := s.RevocationsSince(0); len(revs) != 1 || revs[0].DeviceID != "d" {
		t.Fatalf("unexpected revocations: %#v", revs)
	}
}

func TestGrantRevocationDoesNotRevokeSiblingGrant(t *testing.T) {
	s := NewMemory()
	g1, err := s.AddGrant("alice", "d", "s1", []Permission{PermissionObserve}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddGrant("alice", "d", "s2", []Permission{PermissionObserve}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := s.IssueCapability("alice", "d", "s2", PermissionObserve, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeGrant(g1.ID, "retired"); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateCapability(c, PermissionObserve, time.Now()); err != nil {
		t.Fatalf("sibling grant invalidated: %v", err)
	}
}

func TestAuthenticateCapabilityReturnsCanonicalBinding(t *testing.T) {
	s := NewMemory()
	if _, err := s.AddGrant("alice", "agent-1", "shell", []Permission{PermissionObserve}, nil); err != nil {
		t.Fatal(err)
	}
	capability, err := s.IssueCapability("alice", "agent-1", "shell", PermissionObserve, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := s.AuthenticateCapability(capability.ID, capability.Token, PermissionObserve, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Subject != "alice" || canonical.DeviceID != "agent-1" || canonical.Session != "shell" {
		t.Fatalf("unexpected canonical capability: %#v", canonical)
	}
	if _, err := s.AuthenticateCapability(capability.ID, capability.Token+"x", PermissionObserve, time.Now()); err == nil {
		t.Fatal("modified bearer token authenticated")
	}
}

func TestCapabilityRedemptionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddGrant("alice", "agent-1", "shell", []Permission{PermissionObserve}, nil); err != nil {
		t.Fatal(err)
	}
	capability, err := s.IssueCapability("alice", "agent-1", "shell", PermissionObserve, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemCapability(capability.ID, capability.Token, PermissionObserve, time.Now()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.RedeemCapability(capability.ID, capability.Token, PermissionObserve, time.Now()); err == nil {
		t.Fatal("redeemed capability became reusable after restart")
	}
	if _, err := reopened.AuthenticateCapability(capability.ID, capability.Token, PermissionObserve, time.Now()); err != nil {
		t.Fatalf("redemption incorrectly revoked an active stream: %v", err)
	}
}

func TestPersistenceFailureRollsBackMutation(t *testing.T) {
	s := NewMemory()
	// Renaming a temporary file over an existing directory fails after the
	// in-memory mutation, exercising rollback without test-only hooks.
	s.path = t.TempDir()
	if _, err := s.AddGrant("alice", "agent-1", "shell", []Permission{PermissionObserve}, nil); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(s.state.Grants) != 0 {
		t.Fatalf("failed persistence left %d in-memory grants", len(s.state.Grants))
	}
}

func TestAuditMetadata(t *testing.T) {
	s := NewMemory()
	if err := s.Audit(AuditEvent{Actor: "alice", Session: "s", Action: "attach", Outcome: "allowed"}); err != nil {
		t.Fatal(err)
	}
	entries := s.AuditEvents(10)
	if len(entries) != 1 || entries[0].Action != "attach" || entries[0].ID == "" {
		t.Fatalf("unexpected audit entries: %#v", entries)
	}
}
