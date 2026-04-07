package client

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func tempApprovalStore(t *testing.T) *ApprovalStore {
	t.Helper()
	dir := t.TempDir()
	return NewApprovalStore(filepath.Join(dir, "approvals.json"))
}

func TestApprovalStoreCRUD(t *testing.T) {
	store := tempApprovalStore(t)

	// Initially nothing is approved.
	if store.IsApproved("org/repo", "alice") {
		t.Fatal("expected unapproved")
	}

	// Approve repo.
	if err := store.ApproveRepo("org/repo"); err != nil {
		t.Fatal(err)
	}
	if !store.IsApproved("org/repo", "unknown") {
		t.Fatal("expected repo-approved")
	}

	// Approve sender.
	if err := store.ApproveSender("alice"); err != nil {
		t.Fatal(err)
	}
	if !store.IsApproved("other/repo", "alice") {
		t.Fatal("expected sender-approved")
	}

	// Revoke repo.
	if err := store.RevokeRepo("org/repo"); err != nil {
		t.Fatal(err)
	}
	if store.IsApproved("org/repo", "unknown") {
		t.Fatal("expected revoked")
	}

	// Revoke sender.
	if err := store.RevokeSender("alice"); err != nil {
		t.Fatal(err)
	}
	if store.IsApproved("other/repo", "alice") {
		t.Fatal("expected sender revoked")
	}
}

func TestApprovalStoreWildcard(t *testing.T) {
	store := tempApprovalStore(t)

	if err := store.ApproveRepo("myorg/*"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		repo   string
		expect bool
	}{
		{"myorg/repo-a", true},
		{"myorg/repo-b", true},
		{"other/repo", false},
		{"myorg", false},
	}

	for _, tt := range tests {
		got := store.IsApproved(tt.repo, "anyone")
		if got != tt.expect {
			t.Errorf("IsApproved(%q) = %v, want %v", tt.repo, got, tt.expect)
		}
	}
}

func TestApprovalStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "approvals.json")

	// Create and save.
	store1 := NewApprovalStore(path)
	store1.ApproveRepo("org/repo")
	store1.ApproveSender("bob")

	// Reload from disk.
	store2 := NewApprovalStore(path)
	if !store2.IsApproved("org/repo", "unknown") {
		t.Fatal("repo approval not persisted")
	}
	if !store2.IsApproved("other", "bob") {
		t.Fatal("sender approval not persisted")
	}
}

func TestApprovalStoreMissingFile(t *testing.T) {
	store := NewApprovalStore("/tmp/nonexistent-test-approvals.json")
	if store.IsApproved("any", "any") {
		t.Fatal("expected nothing approved for missing file")
	}
	// Cleanup.
	os.Remove("/tmp/nonexistent-test-approvals.json")
}

func TestApprovalStoreConcurrency(t *testing.T) {
	store := tempApprovalStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		repo := "org/repo"
		go func() {
			defer wg.Done()
			store.ApproveRepo(repo)
		}()
		go func() {
			defer wg.Done()
			store.IsApproved(repo, "sender")
		}()
	}
	wg.Wait()

	if !store.IsApproved("org/repo", "x") {
		t.Fatal("expected approved after concurrent writes")
	}
}

func TestApprovalStoreList(t *testing.T) {
	store := tempApprovalStore(t)
	store.ApproveRepo("org/repo")
	store.ApproveSender("alice")

	out := store.List()
	if len(out) == 0 {
		t.Fatal("expected non-empty list")
	}
}

func TestMatchApprovalPattern(t *testing.T) {
	tests := []struct {
		pattern string
		repo    string
		expect  bool
	}{
		{"org/*", "org/repo", true},
		{"org/*", "org/other", true},
		{"org/*", "other/repo", false},
		{"org/repo", "org/repo", true},
		{"org/repo", "org/other", false},
		{"exact", "exact", true},
		{"exact", "other", false},
	}

	for _, tt := range tests {
		got := matchApprovalPattern(tt.pattern, tt.repo)
		if got != tt.expect {
			t.Errorf("matchApprovalPattern(%q, %q) = %v, want %v", tt.pattern, tt.repo, got, tt.expect)
		}
	}
}
