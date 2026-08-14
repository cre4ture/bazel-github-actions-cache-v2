package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Catalog lists immutable GitHub Actions cache keys. The packed backend uses
// it solely to discover manifest heads; cache bytes are still restored through
// the runner cache-v2 service.
type Catalog interface {
	List(ctx context.Context, keyPrefix string, limit int) ([]string, error)
}

// ActionsCatalog lists cache metadata through the public GitHub REST API.
// A normal GITHUB_TOKEN with actions: read is sufficient. It deliberately
// does not expose deletion or mutation operations.
type ActionsCatalog struct {
	baseURL    *url.URL
	repository string
	token      string
	client     *http.Client
}

// NewActionsCatalog creates a manifest-discovery client from GitHub Actions
// runner environment variables.
func NewActionsCatalog(timeout time.Duration) (*ActionsCatalog, error) {
	repository := os.Getenv("GITHUB_REPOSITORY")
	if len(strings.Split(repository, "/")) != 2 || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return nil, errors.New("GITHUB_REPOSITORY must be owner/repository for packed-cache manifest discovery")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is required for packed-cache manifest discovery")
	}
	baseURLText := os.Getenv("GITHUB_API_URL")
	if baseURLText == "" {
		baseURLText = "https://api.github.com"
	}
	baseURL, err := url.Parse(baseURLText)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid GITHUB_API_URL: %w", err)
	}
	return &ActionsCatalog{
		baseURL:    baseURL,
		repository: repository,
		token:      token,
		client:     &http.Client{Timeout: timeout},
	}, nil
}

type actionsCacheListResponse struct {
	ActionsCaches []struct {
		Key string `json:"key"`
	} `json:"actions_caches"`
}

// List returns at most limit cache keys whose immutable key starts with
// keyPrefix. GitHub's API pagination is followed explicitly so a later writer
// does not silently hide older DAG parents.
func (c *ActionsCatalog) List(ctx context.Context, keyPrefix string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("catalog list limit must be positive")
	}
	page := 1
	keys := make([]string, 0)
	for {
		endpoint := *c.baseURL
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + c.repository + "/actions/caches"
		query := endpoint.Query()
		query.Set("key", keyPrefix)
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("create cache catalog request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, err := c.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("list GitHub Actions caches: %w", err)
		}
		var result actionsCacheListResponse
		decodeErr := json.NewDecoder(response.Body).Decode(&result)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list GitHub Actions caches: HTTP %d", response.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode GitHub Actions cache list: %w", decodeErr)
		}
		for _, entry := range result.ActionsCaches {
			if strings.HasPrefix(entry.Key, keyPrefix) {
				keys = append(keys, entry.Key)
				if len(keys) > limit {
					return nil, fmt.Errorf("manifest catalog exceeds configured limit of %d entries", limit)
				}
			}
		}
		if len(result.ActionsCaches) < 100 {
			return keys, nil
		}
		page++
	}
}
