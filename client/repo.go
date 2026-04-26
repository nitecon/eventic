package client

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nitecon/eventic/protocol"
	"github.com/rs/zerolog/log"
)

// EnsureRepo clones the repo if it doesn't exist, or fetches if it does.
func EnsureRepo(reposDir, repoName, cloneURL string) (string, error) {
	repoPath := filepath.Join(reposDir, repoName)

	if _, err := os.Stat(filepath.Join(repoPath, ".git")); os.IsNotExist(err) {
		log.Debug().Str("repo", repoName).Msg("cloning repo")
		if err := os.MkdirAll(filepath.Dir(repoPath), 0755); err != nil {
			return "", fmt.Errorf("mkdir: %w", err)
		}
		cmd := exec.Command("git", "clone", cloneURL, repoPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git clone: %w", err)
		}
	} else {
		log.Debug().Str("repo", repoName).Msg("fetching repo")
		cmd := exec.Command("git", "-C", repoPath, "fetch", "--all", "--prune")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git fetch: %w", err)
		}
	}

	return repoPath, nil
}

// Checkout switches to the correct ref based on the event.
func Checkout(repoPath string, event protocol.EventMsg) error {
	ref := event.Ref

	// If there's no ref (e.g. workflow_job, workflow_run, check_run events),
	// skip checkout and stay on the current branch.
	if ref == "" && event.GitHubEvent != "pull_request" {
		log.Debug().Str("event", event.GitHubEvent).Msg("no ref provided, skipping checkout")
		return nil
	}

	switch event.GitHubEvent {
	case "pull_request":
		prRef := fmt.Sprintf("pull/%d/head:pr-%d", event.PRNumber, event.PRNumber)
		cmd := exec.Command("git", "-C", repoPath, "fetch", "origin", prRef)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("fetch PR: %w", err)
		}
		ref = fmt.Sprintf("pr-%d", event.PRNumber)

	case "push":
		ref = stripRefPrefix(ref)
		cmd := exec.Command("git", "-C", repoPath, "checkout", ref)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			cmd = exec.Command("git", "-C", repoPath, "checkout", "-b", ref, "origin/"+ref)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("checkout: %w", err)
			}
		}
		pull := exec.Command("git", "-C", repoPath, "pull", "origin", ref)
		pull.Stdout = os.Stdout
		pull.Stderr = os.Stderr
		pull.Run()
		return nil
	}

	cmd := exec.Command("git", "-C", repoPath, "checkout", ref)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func stripRefPrefix(ref string) string {
	prefixes := []string{"refs/heads/", "refs/tags/"}
	for _, p := range prefixes {
		if len(ref) > len(p) && ref[:len(p)] == p {
			return ref[len(p):]
		}
	}
	return ref
}

// CurrentGitState returns the checked-out ref name and commit hash for a repo.
func CurrentGitState(repoPath string) (string, string, error) {
	refCmd := exec.Command("git", "-C", repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	refBytes, err := refCmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git current ref: %w", err)
	}

	hashCmd := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD")
	hashBytes, err := hashCmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git current hash: %w", err)
	}

	return strings.TrimSpace(string(refBytes)), strings.TrimSpace(string(hashBytes)), nil
}
