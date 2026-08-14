package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
)

type memoryCatalog struct {
	backend *memoryBackend
}

func (c memoryCatalog) List(_ context.Context, prefix string, limit int) ([]string, error) {
	c.backend.mu.Lock()
	defer c.backend.mu.Unlock()
	keys := make([]string, 0)
	for key := range c.backend.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		return nil, context.DeadlineExceeded
	}
	return keys, nil
}

var _ cache.Catalog = memoryCatalog{}

func testPackedServer(t *testing.T, backend *memoryBackend) *Server {
	t.Helper()
	return testServer(t, backend, func(cfg *Config) {
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 100
	})
}

func closePackedServer(t *testing.T, server *Server) {
	t.Helper()
	context, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(context); err != nil {
		t.Fatal(err)
	}
}

func TestPackedStoreRoundTripCommitsPackBeforeManifest(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	output := []byte("packed output")
	outputReference := referenceFor(output)
	actionResult := bytesField(2, outputFileProto(outputReference, nil))
	actionDigest := digest([]byte("packed action"))
	if response := putCacheObject(seed, "/cas/"+outputReference.hash, output); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	if response := putCacheObject(seed, "/ac/"+actionDigest, actionResult); response.Code != http.StatusNoContent {
		t.Fatalf("AC PUT = %d", response.Code)
	}
	closePackedServer(t, seed)
	stats := seed.Snapshot()
	if stats.PackUploads != 1 || stats.ManifestUploads != 1 || stats.Uploads != 2 {
		t.Fatalf("unexpected publication stats: %+v", stats)
	}

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusOK {
		t.Fatalf("AC GET = %d, body = %s", response.Code, response.Body.String())
	}
	if response := readCacheObject(restore, "/cas/"+outputReference.hash); response.Code != http.StatusOK || response.Body.String() != string(output) {
		t.Fatalf("CAS GET = %d, body = %q", response.Code, response.Body.String())
	}
	stats = restore.Snapshot()
	if stats.PackDownloads != 1 || stats.Hits != 2 || stats.ValidatedActionResults != 1 {
		t.Fatalf("unexpected restore stats: %+v", stats)
	}
}

func TestPackedStoreMergesConcurrentManifestHeads(t *testing.T) {
	backend := newMemoryBackend()
	writerA := testPackedServer(t, backend)
	writerB := testPackedServer(t, backend)

	for _, test := range []struct {
		server *Server
		body   []byte
	}{
		{writerA, []byte("writer A")},
		{writerB, []byte("writer B")},
	} {
		reference := referenceFor(test.body)
		if response := putCacheObject(test.server, "/cas/"+reference.hash, test.body); response.Code != http.StatusNoContent {
			t.Fatalf("CAS PUT = %d", response.Code)
		}
	}
	closePackedServer(t, writerA)
	closePackedServer(t, writerB)

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	for _, body := range [][]byte{[]byte("writer A"), []byte("writer B")} {
		response := readCacheObject(restore, "/cas/"+digest(body))
		if response.Code != http.StatusOK || response.Body.String() != string(body) {
			t.Fatalf("concurrent CAS restore = %d/%q", response.Code, response.Body.String())
		}
	}
	if restore.Snapshot().ManifestsDiscovered != 2 {
		t.Fatalf("manifest heads were not both discovered: %+v", restore.Snapshot())
	}
}

func TestPackedStoreTreatsMissingPackAsActionCacheMiss(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	actionDigest := digest([]byte("action"))
	if response := putCacheObject(seed, "/ac/"+actionDigest, []byte{0x08, 0x01}); response.Code != http.StatusNoContent {
		t.Fatalf("AC PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	backend.mu.Lock()
	for key := range backend.objects {
		if strings.Contains(key, "-car-pack-v1-") {
			delete(backend.objects, key)
		}
	}
	backend.mu.Unlock()
	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusNotFound {
		t.Fatalf("missing pack action result = %d", response.Code)
	}
}

func TestPackedStoreRejectsConflictingActionResults(t *testing.T) {
	backend := newMemoryBackend()
	writerA := testPackedServer(t, backend)
	writerB := testPackedServer(t, backend)
	actionDigest := digest([]byte("same action"))
	if response := putCacheObject(writerA, "/ac/"+actionDigest, []byte{0x08, 0x01}); response.Code != http.StatusNoContent {
		t.Fatalf("first AC PUT = %d", response.Code)
	}
	if response := putCacheObject(writerB, "/ac/"+actionDigest, []byte{0x08, 0x02}); response.Code != http.StatusNoContent {
		t.Fatalf("second AC PUT = %d", response.Code)
	}
	closePackedServer(t, writerA)
	closePackedServer(t, writerB)

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusNotFound {
		t.Fatalf("conflicting action result = %d", response.Code)
	}
	if restore.Snapshot().ActionDigestConflicts != 1 {
		t.Fatalf("conflict was not reported: %+v", restore.Snapshot())
	}
}
