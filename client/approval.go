package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

// ApprovalStore manages persistent approval of repositories and senders.
type ApprovalStore struct {
	path string
	mu   sync.Mutex
	Data ApprovalData
}

type ApprovalData struct {
	Repos   map[string]bool `json:"repos"`
	Senders map[string]bool `json:"senders"`
}

func NewApprovalStore(path string) *ApprovalStore {
	store := &ApprovalStore{
		path: path,
		Data: ApprovalData{
			Repos:   make(map[string]bool),
			Senders: make(map[string]bool),
		},
	}
	store.load()
	return store
}

func (s *ApprovalStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("failed to load approvals")
		}
		return
	}

	if err := json.Unmarshal(data, &s.Data); err != nil {
		log.Error().Err(err).Msg("failed to parse approvals")
	}
}

// saveLocked persists the current state to disk. Caller must hold s.mu.
func (s *ApprovalStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s.Data, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.path, data, 0644)
}

func (s *ApprovalStore) IsApproved(repo, sender string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Exact repo match.
	if s.Data.Repos[repo] {
		return true
	}

	// Wildcard repo match: "org/*" approves all repos under org.
	for pattern := range s.Data.Repos {
		if matchApprovalPattern(pattern, repo) {
			return true
		}
	}

	// Exact sender match.
	if s.Data.Senders[sender] {
		return true
	}

	return false
}

// matchApprovalPattern checks if repo matches a wildcard pattern.
// Supported: "org/*" matches any repo under "org/".
func matchApprovalPattern(pattern, repo string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == repo
	}
	// "org/*" -> prefix "org/"
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(repo, prefix)
	}
	return false
}

func (s *ApprovalStore) ApproveRepo(repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Data.Repos[repo] = true
	return s.saveLocked()
}

func (s *ApprovalStore) ApproveSender(sender string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Data.Senders[sender] = true
	return s.saveLocked()
}

func (s *ApprovalStore) RevokeRepo(repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Data.Repos, repo)
	return s.saveLocked()
}

func (s *ApprovalStore) RevokeSender(sender string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Data.Senders, sender)
	return s.saveLocked()
}

func (s *ApprovalStore) List() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("Approved repositories:\n")
	if len(s.Data.Repos) == 0 {
		sb.WriteString("  (none)\n")
	}
	for repo := range s.Data.Repos {
		fmt.Fprintf(&sb, "  %s\n", repo)
	}

	sb.WriteString("\nApproved senders:\n")
	if len(s.Data.Senders) == 0 {
		sb.WriteString("  (none)\n")
	}
	for sender := range s.Data.Senders {
		fmt.Fprintf(&sb, "  %s\n", sender)
	}
	return sb.String()
}
