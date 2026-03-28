package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	appVersion "github.com/sailboxhq/sailbox/apps/api/internal/version"
)

type VersionInfo struct {
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	UpdateAvail bool   `json:"update_available"`
	ReleaseURL  string `json:"release_url"`
	Changelog   string `json:"changelog"`
	PublishedAt string `json:"published_at"`
}

type VersionService struct {
	logger *slog.Logger
	mu     sync.RWMutex
	cached *VersionInfo
}

func NewVersionService(logger *slog.Logger) *VersionService {
	svc := &VersionService{logger: logger}
	go svc.periodicCheck()
	return svc
}

func (s *VersionService) GetVersionInfo(ctx context.Context) *VersionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.cached != nil {
		return s.cached
	}

	// Return basic info if no cache yet
	return &VersionInfo{
		Current: appVersion.Version,
		Latest:  appVersion.Version,
	}
}

func (s *VersionService) periodicCheck() {
	// Check immediately on startup
	s.checkForUpdate()

	// Then check every hour
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.checkForUpdate()
	}
}

func (s *VersionService) checkForUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest",
		appVersion.GitHubOwner, appVersion.GitHubRepo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		s.logger.Debug("version check: failed to create request", slog.Any("error", err))
		s.setCached(appVersion.Version, "", false)
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := notifHTTPClient.Do(req) // reuses shared 30s timeout client
	if err != nil {
		s.logger.Debug("version check: request failed", slog.Any("error", err))
		s.setCached(appVersion.Version, "", false)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		s.logger.Debug("version check: non-200 response", slog.Int("status", resp.StatusCode))
		s.setCached(appVersion.Version, "", false)
		return
	}

	var release struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		s.logger.Debug("version check: failed to decode", slog.Any("error", err))
		s.setCached(appVersion.Version, "", false)
		return
	}

	latest := release.TagName

	updateAvail := appVersion.Version != "dev" && shouldUpdate(latest, appVersion.Version)

	s.mu.Lock()
	s.cached = &VersionInfo{
		Current:     appVersion.Version,
		Latest:      latest,
		UpdateAvail: updateAvail,
		ReleaseURL:  release.HTMLURL,
		Changelog:   release.Body,
		PublishedAt: release.PublishedAt,
	}

	s.mu.Unlock()

	if updateAvail {
		s.logger.Info("new version available", slog.String("current", appVersion.Version), slog.String("latest", latest))
	}
}

// shouldUpdate returns true if the user should upgrade.
// True when: latest > current, OR current is a pre-release of the same version.
func shouldUpdate(latest, current string) bool {
	latestClean := strings.TrimPrefix(latest, "v")
	currentClean := strings.TrimPrefix(current, "v")

	// Same exact string — no update
	if latestClean == currentClean {
		return false
	}

	// Current is pre-release (e.g. v1.1.0-rc1), latest is same base version (v1.1.0) — update
	if strings.Contains(currentClean, "-") {
		currentBase := currentClean[:strings.IndexByte(currentClean, '-')]
		latestBase := latestClean
		if idx := strings.IndexByte(latestBase, '-'); idx != -1 {
			latestBase = latestBase[:idx]
		}
		if currentBase == latestBase && !strings.Contains(latestClean, "-") {
			return true
		}
	}

	return isNewer(latest, current)
}

// isNewer returns true if latest is a higher semver than current.
func isNewer(latest, current string) bool {
	parse := func(v string) (int, int, int) {
		v = strings.TrimPrefix(v, "v")
		// Strip pre-release suffix (e.g. "-rc1")
		if idx := strings.IndexByte(v, '-'); idx != -1 {
			v = v[:idx]
		}
		parts := strings.SplitN(v, ".", 3)
		a, _ := strconv.Atoi(parts[0])
		b, c := 0, 0
		if len(parts) > 1 {
			b, _ = strconv.Atoi(parts[1])
		}
		if len(parts) > 2 {
			c, _ = strconv.Atoi(parts[2])
		}
		return a, b, c
	}
	la, lb, lc := parse(latest)
	ca, cb, cc := parse(current)
	if la != ca {
		return la > ca
	}
	if lb != cb {
		return lb > cb
	}
	return lc > cc
}

func (s *VersionService) setCached(current, latest string, updateAvail bool) {
	s.mu.Lock()
	s.cached = &VersionInfo{
		Current:     current,
		Latest:      latest,
		UpdateAvail: updateAvail,
	}

	s.mu.Unlock()
}
