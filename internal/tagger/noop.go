//go:build !tagger

package tagger

import (
	"context"
	"errors"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
)

// CheckProviderAvailable always errors here; inference is compiled out.
func CheckProviderAvailable(_ string) error {
	return errors.New("auto-tagger disabled (built without -tags tagger)")
}

func IsAvailable(cfg *config.Config) bool {
	return cfg.Tagger.RemoteClient.URL != "" && cfg.Tagger.RemoteClient.Token != ""
}

func buildSupportsInference() bool { return false }

func UnavailableReason(cfg *config.Config) string {
	if IsAvailable(cfg) {
		return ""
	}
	return "inference disabled (built without -tags tagger)"
}

// AvailableTaggers lists every configured tagger as unavailable because
// inference is disabled at build time.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	list := DiscoverTaggers(cfg)
	for i := range list {
		list[i].Available = false
		list[i].Reason = "inference disabled (built without -tags tagger)"
	}
	return list
}

// RunWithTaggers is the no-op stub matching the tagger build signature.
// When a remote tagger is configured, it delegates to runRemoteTaggers.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string, mangaCacheDir string) (int, error) {
	if cfg.Tagger.RemoteClient.URL != "" && cfg.Tagger.RemoteClient.Token != "" {
		return runRemoteTaggers(ctx, database, cfg, ids, taggers, mgr, provider, mangaCacheDir)
	}
	return 0, nil
}

// ReleaseIdle is a no-op stub on the non-tagger build; nothing is
// cached when inference is compiled out.
func ReleaseIdle(_ time.Duration) bool { return false }

// ReleaseAll is a no-op stub on the non-tagger build.
func ReleaseAll() {}

// Status reports "not loaded" since the non-tagger build never caches.
func Status() CacheStatus { return CacheStatus{} }

// SubmitRemoteImage is a no-op stub on the non-tagger build; the noop
// build never runs a local backend, it only consumes a remote one.
func SubmitRemoteImage(_ context.Context, _ RemoteRunParams, _ BackendImageRequest, _ string) (string, error) {
	return "", errors.New("not built with -tags tagger, inference unavailable")
}

// RemoteQueueStatus is a no-op stub on the non-tagger build.
func RemoteQueueStatus() (int, int, int) { return 0, 0, 0 }

// RemoteDrainResults is a no-op stub on the non-tagger build.
func RemoteDrainResults(_ string, _ int64, _ time.Duration) (int64, []RemoteDrainedResult, error) {
	return 0, nil, nil
}

// SetRemoteQueueCapacity is a no-op stub on the non-tagger build.
func SetRemoteQueueCapacity(_ int) {}
