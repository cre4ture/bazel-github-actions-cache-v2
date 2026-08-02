package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
)

var (
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	urlPattern    = regexp.MustCompile(`https?://[^\s]+`)
	jwtPattern    = regexp.MustCompile(`[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)
)

type Config struct {
	Backend          cache.Backend
	CacheDir         string
	KeyPrefix        string
	WriteEnabled     bool
	FailOpen         bool
	MaxBlobSize      int64
	MaxConcurrent    int
	UploadsPerMinute int
	BackendTimeout   time.Duration
	ShutdownToken    string
	Shutdown         func()
	Logger           *log.Logger
}

type object struct {
	path string
	size int64
}

type publication struct {
	done chan struct{}
	err  error
}

type Server struct {
	cfg            Config
	stats          counters
	objectsMu      sync.RWMutex
	objects        map[string]object
	publicationsMu sync.Mutex
	published      map[string]struct{}
	publishing     map[string]*publication
	sem            chan struct{}
	limiter        *intervalLimiter
}

func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("backend is required")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("cache directory is required")
	}
	if !prefixPattern.MatchString(cfg.KeyPrefix) {
		return nil, errors.New("key prefix must match [A-Za-z0-9][A-Za-z0-9._-]{0,79}")
	}
	if cfg.MaxBlobSize <= 0 {
		return nil, errors.New("max blob size must be positive")
	}
	if cfg.MaxConcurrent <= 0 {
		return nil, errors.New("max concurrent operations must be positive")
	}
	if cfg.UploadsPerMinute <= 0 || cfg.UploadsPerMinute > 199 {
		return nil, errors.New("uploads per minute must be between 1 and 199")
	}
	if cfg.BackendTimeout <= 0 {
		return nil, errors.New("backend timeout must be positive")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	return &Server{
		cfg:        cfg,
		objects:    make(map[string]object),
		published:  make(map[string]struct{}),
		publishing: make(map[string]*publication),
		sem:        make(chan struct{}, cfg.MaxConcurrent),
		limiter:    newIntervalLimiter(cfg.UploadsPerMinute),
	}, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) Snapshot() Stats {
	return s.stats.snapshot()
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ready":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "ready\n")
		}
		return
	case "/stats":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body := s.Snapshot().JSON()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
		return
	case "/shutdown":
		s.handleShutdown(w, r)
		return
	}

	s.stats.requests.Add(1)
	kind, digest, ok := parseObjectPath(r.URL.Path)
	if !ok {
		s.reject(w, "path must be /cas/<lowercase-sha256> or /ac/<lowercase-sha256>", http.StatusBadRequest)
		return
	}
	if kind == "cas" && digest == emptySHA256Digest && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.handleImplicitEmptyCASRead(w, r)
		return
	}
	key := s.cfg.KeyPrefix + "-" + kind + "-" + digest
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		s.handleRead(w, r, key, kind, digest)
	case http.MethodPut:
		s.handlePut(w, r, key, kind, digest)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		s.reject(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleImplicitEmptyCASRead(w http.ResponseWriter, r *http.Request) {
	s.stats.hits.Add(1)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provided := r.Header.Get("X-Shutdown-Token")
	expected := s.cfg.ShutdownToken
	if expected == "" || len(provided) != len(expected) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	if s.cfg.Shutdown != nil {
		go s.cfg.Shutdown()
	}
}

func parseObjectPath(path string) (kind, digest string, ok bool) {
	if strings.Contains(path, "//") || strings.HasSuffix(path, "/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || (parts[0] != "cas" && parts[0] != "ac") {
		return "", "", false
	}
	if !digestPattern.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, key, kind, digest string) {
	obj, found, err := s.resolve(r.Context(), key, kind, digest)
	if err != nil {
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("backend load failed for %s/%s: %s", kind, digest, safeError(err))
		if s.cfg.FailOpen {
			s.stats.misses.Add(1)
			http.NotFound(w, r)
		} else {
			http.Error(w, "cache backend unavailable", http.StatusBadGateway)
		}
		return
	}
	if !found {
		s.stats.misses.Add(1)
		http.NotFound(w, r)
		return
	}
	if kind == "ac" {
		if err := s.validateActionResult(r.Context(), obj); err != nil {
			switch {
			case errors.Is(err, errIncompleteActionResult):
				s.stats.incompleteActionResults.Add(1)
				s.stats.misses.Add(1)
				s.cfg.Logger.Printf("action result ac/%s is incomplete: %s", digest, safeError(err))
				http.NotFound(w, r)
			case errors.Is(err, errInvalidActionResult):
				s.stats.invalidActionResults.Add(1)
				s.stats.misses.Add(1)
				s.cfg.Logger.Printf("action result ac/%s is invalid: %s", digest, safeError(err))
				http.NotFound(w, r)
			default:
				s.stats.backendLoadErrors.Add(1)
				s.cfg.Logger.Printf("action result ac/%s validation failed: %s", digest, safeError(err))
				if s.cfg.FailOpen {
					s.stats.misses.Add(1)
					http.NotFound(w, r)
				} else {
					http.Error(w, "cache backend unavailable", http.StatusBadGateway)
				}
			}
			return
		}
		s.stats.validatedActionResults.Add(1)
	}

	file, err := os.Open(obj.path)
	if err != nil {
		s.stats.backendLoadErrors.Add(1)
		s.cfg.Logger.Printf("open local cache object %s/%s: %v", kind, digest, err)
		if s.cfg.FailOpen {
			s.stats.misses.Add(1)
			http.NotFound(w, r)
		} else {
			http.Error(w, "local cache unavailable", http.StatusInternalServerError)
		}
		return
	}
	defer file.Close()

	s.stats.hits.Add(1)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(obj.size, 10))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, file)
	s.stats.bytesServed.Add(uint64(n))
	if err != nil {
		s.cfg.Logger.Printf("serve cache object %s/%s: %v", kind, digest, err)
	}
}

func (s *Server) resolve(ctx context.Context, key, kind, digest string) (object, bool, error) {
	s.objectsMu.RLock()
	obj, ok := s.objects[key]
	s.objectsMu.RUnlock()
	if ok {
		return obj, true, nil
	}

	if err := s.acquire(ctx); err != nil {
		return object{}, false, err
	}
	defer s.release()

	// Re-check after waiting for another backend operation.
	s.objectsMu.RLock()
	obj, ok = s.objects[key]
	s.objectsMu.RUnlock()
	if ok {
		return obj, true, nil
	}

	file, err := os.CreateTemp(s.cfg.CacheDir, "download-*")
	if err != nil {
		return object{}, false, fmt.Errorf("create download spool: %w", err)
	}
	path := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()

	limited := &maxWriter{writer: file, remaining: s.cfg.MaxBlobSize}
	backendCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	found, err := s.cfg.Backend.Load(backendCtx, key, limited)
	if err != nil {
		return object{}, false, err
	}
	if !found {
		return object{}, false, nil
	}
	s.stats.backendDownloads.Add(1)
	if err := file.Sync(); err != nil {
		return object{}, false, fmt.Errorf("sync download spool: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return object{}, false, fmt.Errorf("stat download spool: %w", err)
	}
	if kind == "cas" {
		actual, err := hashFile(file)
		if err != nil {
			return object{}, false, err
		}
		if actual != digest {
			return object{}, false, fmt.Errorf("CAS integrity check failed: expected %s, got %s", digest, actual)
		}
	}
	obj = object{path: path, size: info.Size()}
	s.objectsMu.Lock()
	if existing, exists := s.objects[key]; exists {
		obj = existing
	} else {
		s.objects[key] = obj
		keep = true
	}
	s.objectsMu.Unlock()
	return obj, true, nil
}

func (s *Server) exists(ctx context.Context, key string) (bool, error) {
	s.objectsMu.RLock()
	_, ok := s.objects[key]
	s.objectsMu.RUnlock()
	if ok {
		return true, nil
	}
	return s.backendExists(ctx, key)
}

func (s *Server) backendExists(ctx context.Context, key string) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()

	backendCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	s.stats.backendExistenceChecks.Add(1)
	return s.cfg.Backend.Exists(backendCtx, key)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, key, kind, digest string) {
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		s.reject(w, "content encoding is unsupported", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength < 0 {
		s.reject(w, "Content-Length is required", http.StatusLengthRequired)
		return
	}
	if r.ContentLength > s.cfg.MaxBlobSize {
		s.reject(w, "object exceeds configured maximum size", http.StatusRequestEntityTooLarge)
		return
	}

	file, err := os.CreateTemp(s.cfg.CacheDir, "upload-*")
	if err != nil {
		http.Error(w, "cannot create upload spool", http.StatusInternalServerError)
		return
	}
	path := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()

	hasher := sha256.New()
	writer := io.Writer(file)
	if kind == "cas" {
		writer = io.MultiWriter(file, hasher)
	}
	n, err := io.Copy(writer, io.LimitReader(r.Body, r.ContentLength+1))
	s.stats.bytesReceived.Add(uint64(n))
	if err != nil {
		s.reject(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if n != r.ContentLength {
		s.reject(w, "body size does not match Content-Length", http.StatusBadRequest)
		return
	}
	if kind == "cas" {
		actual := hex.EncodeToString(hasher.Sum(nil))
		if actual != digest {
			s.reject(w, "CAS digest does not match request body", http.StatusUnprocessableEntity)
			return
		}
	}
	if err := file.Sync(); err != nil {
		http.Error(w, "cannot sync upload spool", http.StatusInternalServerError)
		return
	}

	s.objectsMu.Lock()
	if _, exists := s.objects[key]; !exists {
		s.objects[key] = object{path: path, size: n}
		keep = true
	}
	s.objectsMu.Unlock()

	if !s.cfg.WriteEnabled {
		s.stats.discardedUploads.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if kind == "ac" {
		if err := s.validateActionResultForPublication(r.Context(), object{path: path, size: n}); err != nil {
			switch {
			case errors.Is(err, errIncompleteActionResult):
				s.stats.incompleteActionResults.Add(1)
				s.stats.skippedActionResultUploads.Add(1)
				s.cfg.Logger.Printf("not publishing incomplete action result ac/%s: %s", digest, safeError(err))
				w.WriteHeader(http.StatusNoContent)
			case errors.Is(err, errInvalidActionResult):
				s.stats.invalidActionResults.Add(1)
				s.stats.skippedActionResultUploads.Add(1)
				s.cfg.Logger.Printf("not publishing invalid action result ac/%s: %s", digest, safeError(err))
				w.WriteHeader(http.StatusNoContent)
			default:
				s.stats.backendLoadErrors.Add(1)
				s.cfg.Logger.Printf("action result ac/%s publication validation failed: %s", digest, safeError(err))
				if s.cfg.FailOpen {
					s.stats.skippedActionResultUploads.Add(1)
					w.WriteHeader(http.StatusNoContent)
				} else {
					http.Error(w, "cache backend unavailable", http.StatusBadGateway)
				}
			}
			return
		}
		s.stats.validatedActionResults.Add(1)
	}

	deduplicated, err := s.publishOnce(r.Context(), key, file, n)
	if err != nil {
		s.handleSaveError(w, kind, digest, err)
		return
	}
	if deduplicated {
		s.stats.deduplicatedUploads.Add(1)
	} else {
		s.stats.uploads.Add(1)
	}
	w.WriteHeader(http.StatusNoContent)
}

// publishOnce coalesces identical immutable keys for the lifetime of the
// server. Only the request which creates the publication reaches the backend
// and consumes an upload-rate-limit slot. A failed publication is not cached,
// so a later request can retry it.
func (s *Server) publishOnce(ctx context.Context, key string, file *os.File, size int64) (bool, error) {
	for {
		s.publicationsMu.Lock()
		if _, ok := s.published[key]; ok {
			s.publicationsMu.Unlock()
			return true, nil
		}
		if ongoing, ok := s.publishing[key]; ok {
			done := ongoing.done
			s.publicationsMu.Unlock()
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-done:
				if ongoing.err == nil {
					return true, nil
				}
				// The publishing request failed. Try this request as a new
				// leader so transient backend failures remain retryable.
				continue
			}
		}

		ongoing := &publication{done: make(chan struct{})}
		s.publishing[key] = ongoing
		s.publicationsMu.Unlock()

		err := s.publish(ctx, key, file, size)

		s.publicationsMu.Lock()
		if err == nil {
			s.published[key] = struct{}{}
		}
		ongoing.err = err
		delete(s.publishing, key)
		close(ongoing.done)
		s.publicationsMu.Unlock()
		return false, err
	}
}

func (s *Server) publish(ctx context.Context, key string, file *os.File, size int64) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if waited, err := s.limiter.wait(ctx); err != nil {
		return err
	} else if waited {
		s.stats.throttleWaits.Add(1)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	backendCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	return s.cfg.Backend.Save(backendCtx, key, file, size)
}

func (s *Server) handleSaveError(w http.ResponseWriter, kind, digest string, err error) {
	s.stats.backendSaveErrors.Add(1)
	s.cfg.Logger.Printf("backend save failed for %s/%s: %s", kind, digest, safeError(err))
	if s.cfg.FailOpen {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "cache backend unavailable", http.StatusBadGateway)
}

func (s *Server) reject(w http.ResponseWriter, message string, status int) {
	s.stats.rejectedRequests.Add(1)
	http.Error(w, message, status)
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() {
	<-s.sem
}

func hashFile(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek cache object: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash cache object: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind cache object: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type maxWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *maxWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("download exceeds configured maximum size")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func WriteStatsFile(path string, stats Stats) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := urlPattern.ReplaceAllString(err.Error(), "[redacted-url]")
	message = jwtPattern.ReplaceAllString(message, "[redacted-token]")
	const maxLength = 1000
	if len(message) > maxLength {
		message = message[:maxLength] + "…"
	}
	return message
}

func SafeCacheDir(base string) (string, error) {
	if base != "" {
		absolute, err := filepath.Abs(base)
		if err != nil {
			return "", err
		}
		return absolute, nil
	}
	return os.MkdirTemp("", "bazel-gha-cache-v2-*")
}
