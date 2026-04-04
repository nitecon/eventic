package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

const repoCheckInterval = 5 * time.Minute

// StartRepoChecker runs a background loop that periodically walks the repos
// directory and verifies every cloned repository is healthy. Repos that are
// missing a .git directory or fail a basic git sanity check are re-cloned on
// the default branch. Repos already checked out on a branch are left as-is.
//
// Repos are checked one at a time to avoid overwhelming the system.
func StartRepoChecker(ctx context.Context, cfg Config) {
	log.Info().Str("repos_dir", cfg.ReposDir).Msg("repo checker started")

	// Run once immediately at startup.
	checkRepos(ctx, cfg)

	ticker := time.NewTicker(repoCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkRepos(ctx, cfg)
		}
	}
}

// checkRepos walks <repos_dir>/<owner>/<repo> and verifies each one.
func checkRepos(ctx context.Context, cfg Config) {
	log.Debug().Msg("running repo health check")

	owners, err := os.ReadDir(cfg.ReposDir)
	if err != nil {
		log.Error().Err(err).Msg("failed to read repos directory")
		return
	}

	checked := 0
	repaired := 0

	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}

		ownerPath := filepath.Join(cfg.ReposDir, owner.Name())
		repos, err := os.ReadDir(ownerPath)
		if err != nil {
			log.Error().Err(err).Str("owner", owner.Name()).Msg("failed to read owner directory")
			continue
		}

		for _, repo := range repos {
			if !repo.IsDir() {
				continue
			}

			select {
			case <-ctx.Done():
				return
			default:
			}

			repoName := owner.Name() + "/" + repo.Name()
			repoPath := filepath.Join(ownerPath, repo.Name())
			checked++

			if repairIfNeeded(ctx, repoName, repoPath) {
				repaired++
			}
		}
	}

	log.Info().Int("checked", checked).Int("repaired", repaired).Msg("repo health check complete")
}

// repairIfNeeded checks a single repo and re-clones it if broken.
// Returns true if a repair was performed. Acquires the per-repo lock
// so it won't collide with event processing.
func repairIfNeeded(ctx context.Context, repoName, repoPath string) bool {
	repoLocks.Lock(repoName)
	defer repoLocks.Unlock(repoName)

	gitDir := filepath.Join(repoPath, ".git")

	// Check 1: .git directory must exist.
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		log.Warn().Str("repo", repoName).Msg("missing .git directory, re-cloning")
		return reclone(ctx, repoName, repoPath)
	}

	// Check 2: git must recognise this as a valid work tree.
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--is-inside-work-tree")
	if err := cmd.Run(); err != nil {
		log.Warn().Str("repo", repoName).Err(err).Msg("corrupt git repo, re-cloning")
		return reclone(ctx, repoName, repoPath)
	}

	log.Debug().Str("repo", repoName).Msg("repo healthy")
	return false
}

// reclone removes a broken repo directory and clones it fresh.
func reclone(ctx context.Context, repoName, repoPath string) bool {
	cloneURL := fmt.Sprintf("https://github.com/%s.git", repoName)

	if err := os.RemoveAll(repoPath); err != nil {
		log.Error().Err(err).Str("repo", repoName).Msg("failed to remove broken repo")
		return false
	}

	if err := os.MkdirAll(filepath.Dir(repoPath), 0755); err != nil {
		log.Error().Err(err).Str("repo", repoName).Msg("failed to create parent directory")
		return false
	}

	log.Info().Str("repo", repoName).Str("url", cloneURL).Msg("re-cloning repo")
	cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, repoPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Error().Err(err).Str("repo", repoName).Msg("re-clone failed")
		return false
	}

	log.Info().Str("repo", repoName).Msg("repo repaired successfully")
	return true
}
