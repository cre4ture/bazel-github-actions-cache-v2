package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryBackend struct {
	mu        sync.Mutex
	objects   map[string][]byte
	loadErr   error
	existsErr error
	saveErr   error
	saves     int
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{objects: make(map[string][]byte)}
}

func (b *memoryBackend) Load(_ context.Context, key string, dst io.Writer) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.loadErr != nil {
		return false, b.loadErr
	}
	data, ok := b.objects[key]
	if !ok {
		return false, nil
	}
	_, err := dst.Write(data)
	return true, err
}

func (b *memoryBackend) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.existsErr != nil {
		return false, b.existsErr
	}
	_, ok := b.objects[key]
	return ok, nil
}

func (b *memoryBackend) Save(_ context.Context, key string, src *os.File, size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saves++
	if b.saveErr != nil {
		return b.saveErr
	}
	data := make([]byte, size)
	if _, err := src.ReadAt(data, 0); err != nil && err != io.EOF {
		return err
	}
	b.objects[key] = data
	return nil
}

func testServer(t *testing.T, backend *memoryBackend, mutate func(*Config)) *Server {
	t.Helper()
	cfg := Config{
		Backend:          backend,
		CacheDir:         t.TempDir(),
		KeyPrefix:        "test-v1",
		WriteEnabled:     true,
		FailOpen:         true,
		MaxBlobSize:      1024,
		MaxConcurrent:    2,
		UploadsPerMinute: 199,
		BackendTimeout:   time.Second,
		Logger:           log.New(io.Discard, "", 0),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestCASPutHeadGet(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)
	data := []byte("hello bazel")
	path := "/cas/" + digest(data)

	put := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(data))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, put)
	if response.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, body = %s", response.Code, response.Body)
	}

	head := httptest.NewRequest(http.MethodHead, path, nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, head)
	if response.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q", got)
	}

	get := httptest.NewRequest(http.MethodGet, path, nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, get)
	if response.Code != http.StatusOK || response.Body.String() != string(data) {
		t.Fatalf("GET status/body = %d/%q", response.Code, response.Body.String())
	}
	stats := server.Snapshot()
	if stats.Uploads != 1 || stats.Hits != 2 || stats.BytesServed != uint64(len(data)) {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestEmptyCASDigestIsServedImplicitly(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	path := "/cas/" + digest(nil)

	for _, method := range []string{http.MethodHead, http.MethodGet} {
		request := httptest.NewRequest(method, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Content-Length") != "0" || response.Body.Len() != 0 {
			t.Fatalf("%s status/length/body = %d/%q/%q", method, response.Code, response.Header().Get("Content-Length"), response.Body.Bytes())
		}
	}
	stats := server.Snapshot()
	if stats.Hits != 2 || stats.BackendDownloads != 0 || stats.BackendExistenceChecks != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestCASRejectsDigestMismatch(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	request := httptest.NewRequest(http.MethodPut, "/cas/"+strings.Repeat("0", 64), strings.NewReader("not zero"))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestStrictRequestValidation(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	tests := []struct {
		name   string
		path   string
		length int64
		status int
	}{
		{"uppercase digest", "/cas/" + strings.Repeat("A", 64), 0, http.StatusBadRequest},
		{"extra path", "/cas/" + strings.Repeat("a", 64) + "/x", 0, http.StatusBadRequest},
		{"chunked upload", "/ac/" + strings.Repeat("a", 64), -1, http.StatusLengthRequired},
		{"too large", "/ac/" + strings.Repeat("a", 64), 1025, http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, test.path, bytes.NewReader(nil))
			request.ContentLength = test.length
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
		})
	}
}

func TestReadOnlyValidatesAndDiscardsBackendUpload(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, func(cfg *Config) {
		cfg.WriteEnabled = false
	})
	data := []byte("read only")
	request := httptest.NewRequest(http.MethodPut, "/cas/"+digest(data), bytes.NewReader(data))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if backend.saves != 0 || server.Snapshot().DiscardedUploads != 1 {
		t.Fatalf("read-only upload reached backend")
	}
}

func TestFailOpenBackendErrors(t *testing.T) {
	backend := newMemoryBackend()
	backend.loadErr = errors.New("unavailable")
	server := testServer(t, backend, nil)
	request := httptest.NewRequest(http.MethodGet, "/ac/"+strings.Repeat("a", 64), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}

	backend.loadErr = nil
	backend.saveErr = errors.New("unavailable")
	data := []byte("result")
	request = httptest.NewRequest(http.MethodPut, "/cas/"+digest(data), bytes.NewReader(data))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want soft-success", response.Code)
	}
	if server.Snapshot().BackendSaveErrors != 1 {
		t.Fatalf("backend save failure was not counted: %+v", server.Snapshot())
	}
}

func TestStrictBackendErrors(t *testing.T) {
	backend := newMemoryBackend()
	backend.loadErr = errors.New("unavailable")
	server := testServer(t, backend, func(cfg *Config) {
		cfg.FailOpen = false
	})
	request := httptest.NewRequest(http.MethodGet, "/ac/"+strings.Repeat("a", 64), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}

func TestCorruptCASBackendIsMiss(t *testing.T) {
	backend := newMemoryBackend()
	key := "test-v1-cas-" + strings.Repeat("a", 64)
	backend.objects[key] = []byte("corrupt")
	server := testServer(t, backend, nil)
	request := httptest.NewRequest(http.MethodGet, "/cas/"+strings.Repeat("a", 64), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if server.Snapshot().BackendLoadErrors != 1 {
		t.Fatalf("integrity failure was not counted")
	}
}

func TestBackendDownloadIsMeasured(t *testing.T) {
	backend := newMemoryBackend()
	data := []byte("persisted")
	hash := digest(data)
	backend.objects["test-v1-cas-"+hash] = data
	server := testServer(t, backend, nil)
	request := httptest.NewRequest(http.MethodGet, "/cas/"+hash, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != string(data) {
		t.Fatalf("status/body = %d/%q", response.Code, response.Body.String())
	}
	if server.Snapshot().BackendDownloads != 1 {
		t.Fatalf("backend download was not counted: %+v", server.Snapshot())
	}
}

func TestShutdownToken(t *testing.T) {
	called := make(chan struct{}, 1)
	server := testServer(t, newMemoryBackend(), func(cfg *Config) {
		cfg.ShutdownToken = "secret"
		cfg.Shutdown = func() { called <- struct{}{} }
	})
	request := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	request.Header.Set("X-Shutdown-Token", "wrong")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong token status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	request.Header.Set("X-Shutdown-Token", "secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("correct token status = %d", response.Code)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	_, err := New(Config{
		Backend:          newMemoryBackend(),
		CacheDir:         t.TempDir(),
		KeyPrefix:        "../unsafe",
		MaxBlobSize:      1,
		MaxConcurrent:    1,
		UploadsPerMinute: 199,
		BackendTimeout:   time.Second,
	})
	if err == nil {
		t.Fatal("unsafe key prefix accepted")
	}
}

func TestIntervalLimiterHonorsCancellation(t *testing.T) {
	limiter := newIntervalLimiter(1)
	if waited, err := limiter.wait(context.Background()); err != nil || waited {
		t.Fatalf("first wait = %t, %v", waited, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if waited, err := limiter.wait(ctx); !waited || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second wait = %t, %v", waited, err)
	}
}

func TestSafeErrorRedactsURLsAndTokens(t *testing.T) {
	raw := "request https://blob.example.invalid/object?sig=secret failed with " +
		"aaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbb.cccccccccccccccc"
	got := safeError(errors.New(raw))
	if strings.Contains(got, "secret") || strings.Contains(got, "aaaaaaaa") {
		t.Fatalf("secret was not redacted: %q", got)
	}
}
