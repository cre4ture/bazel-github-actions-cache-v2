package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	actionscache "github.com/tonistiigi/go-actions-cache"
)

// Backend is the small persistence interface used by the HTTP cache server.
// Implementations must treat keys as immutable.
type Backend interface {
	Load(ctx context.Context, key string, dst io.Writer) (bool, error)
	Save(ctx context.Context, key string, src *os.File, size int64) error
}

// ActionsBackend stores one immutable object per GitHub Actions cache entry.
type ActionsBackend struct {
	cache *actionscache.Cache
}

// NewActionsBackend creates a backend from the GitHub-hosted runner environment.
// Legacy Actions cache v1 is intentionally rejected.
func NewActionsBackend(timeout time.Duration) (*ActionsBackend, error) {
	if !strings.EqualFold(os.Getenv("ACTIONS_CACHE_SERVICE_V2"), "true") {
		return nil, errors.New("ACTIONS_CACHE_SERVICE_V2 must be true; the legacy cache v1 API is unsupported")
	}
	if os.Getenv("ACTIONS_RESULTS_URL") == "" {
		return nil, errors.New("ACTIONS_RESULTS_URL is not set")
	}
	if os.Getenv("ACTIONS_RUNTIME_TOKEN") == "" {
		return nil, errors.New("ACTIONS_RUNTIME_TOKEN is not set")
	}

	c, err := actionscache.TryEnv(actionscache.Opt{
		Client:    &http.Client{},
		Timeout:   timeout,
		UserAgent: "bazel-github-actions-cache-v2/0.1",
	})
	if err != nil {
		return nil, fmt.Errorf("initialize GitHub Actions cache v2: %w", err)
	}
	if c == nil {
		return nil, errors.New("GitHub Actions cache credentials are unavailable")
	}
	if !c.IsV2 {
		return nil, errors.New("refusing to use the legacy GitHub Actions cache v1 API")
	}
	return &ActionsBackend{cache: c}, nil
}

func (b *ActionsBackend) Load(ctx context.Context, key string, dst io.Writer) (bool, error) {
	entry, err := b.cache.Load(ctx, key)
	if err != nil {
		return false, fmt.Errorf("load cache entry: %w", err)
	}
	if entry == nil {
		return false, nil
	}
	if err := entry.WriteTo(ctx, dst); err != nil {
		return false, fmt.Errorf("download cache entry: %w", err)
	}
	return true, nil
}

func (b *ActionsBackend) Save(ctx context.Context, key string, src *os.File, size int64) error {
	blob := fileBlob{file: src, size: size}
	if err := b.cache.Save(ctx, key, blob); err == nil {
		return nil
	} else {
		// Cache keys are immutable. A parallel job may have won the reservation,
		// and cache-v2 finalization can be briefly eventually consistent.
		originalErr := err
		for _, delay := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, time.Second} {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("save cache entry: %w", originalErr)
			case <-timer.C:
			}
			entry, loadErr := b.cache.Load(ctx, key)
			if loadErr == nil && entry != nil {
				return nil
			}
		}
		return fmt.Errorf("save cache entry: %w", originalErr)
	}
}

type fileBlob struct {
	file *os.File
	size int64
}

func (b fileBlob) ReadAt(p []byte, off int64) (int, error) {
	return b.file.ReadAt(p, off)
}

func (b fileBlob) Size() int64 {
	return b.size
}

// The caller owns the spool file.
func (fileBlob) Close() error {
	return nil
}
