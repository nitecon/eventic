package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/minio/selfupdate"
	"github.com/rs/zerolog/log"
)

// Version is set at build time via -ldflags "-X main.version=...".
// It is copied from the main package at startup.
var Version = "dev"

const (
	updateCheckInterval = 5 * time.Minute
	releaseAPIURL       = "https://api.github.com/repos/nitecon/eventic/releases/latest"
	releaseDownloadURL  = "https://github.com/nitecon/eventic/releases/download/%s/eventic-client-%s-%s"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
}

// StartAutoUpdater runs a background loop that checks for new releases every
// 5 minutes. When a newer version is found it replaces the running binary and
// exits so systemd (or the parent supervisor) can restart the service.
func StartAutoUpdater(ctx context.Context) {
	log.Info().Str("version", Version).Msg("auto-updater started")

	// Check once immediately at startup.
	if updated := checkAndUpdate(ctx); updated {
		return
	}

	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if updated := checkAndUpdate(ctx); updated {
				return
			}
		}
	}
}

// checkAndUpdate checks for a newer release and applies it. Returns true if
// the binary was replaced and the process should exit.
func checkAndUpdate(ctx context.Context) bool {
	latest, err := getLatestVersion(ctx)
	if err != nil {
		log.Error().Err(err).Msg("failed to check for updates")
		return false
	}

	latestClean := strings.TrimPrefix(latest, "v")
	currentClean := strings.TrimPrefix(Version, "v")

	if latestClean == currentClean {
		log.Debug().Str("version", Version).Msg("already up to date")
		return false
	}

	log.Info().
		Str("current", Version).
		Str("latest", latest).
		Msg("update available, downloading")

	if err := applyUpdate(ctx, latest); err != nil {
		log.Error().Err(err).Msg("failed to apply update")
		return false
	}

	log.Info().Str("version", latest).Msg("update applied, exiting for restart")
	os.Exit(0)
	return true // unreachable, but keeps the compiler happy
}

func getLatestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPIURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return release.TagName, nil
}

func applyUpdate(ctx context.Context, tag string) error {
	url := fmt.Sprintf(releaseDownloadURL, tag, runtime.GOOS, runtime.GOARCH)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s returned %d", url, resp.StatusCode)
	}

	return selfupdate.Apply(resp.Body, selfupdate.Options{})
}
