package server

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestParseActionResultCollectsOnlyExternalCASReferences(t *testing.T) {
	direct := referenceFor([]byte("direct output"))
	inline := referenceFor([]byte("inline output"))
	tree := referenceFor([]byte("tree"))
	root := referenceFor([]byte("root"))
	stdout := referenceFor([]byte("inline stdout"))
	stderr := referenceFor([]byte("external stderr"))

	actionResult := concatProto(
		bytesField(2, outputFileProto(direct, nil)),
		bytesField(2, outputFileProto(inline, []byte("inline output"))),
		bytesField(3, outputDirectoryProto(tree, root)),
		bytesField(5, []byte("inline stdout")),
		bytesField(6, digestProto(stdout)),
		bytesField(8, digestProto(stderr)),
	)

	references, err := parseActionResult(actionResult)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, references.blobs.values, direct, stderr)
	assertReferences(t, references.trees.values, tree)
	assertReferences(t, references.directories.values, root)
}

func TestParseActionResultRejectsMalformedDigest(t *testing.T) {
	malformedDigest := concatProto(
		bytesField(1, []byte(strings.Repeat("A", 64))),
		varintField(2, 1),
	)
	actionResult := bytesField(2, bytesField(2, malformedDigest))
	if _, err := parseActionResult(actionResult); err == nil {
		t.Fatal("uppercase digest was accepted")
	}
}

func TestParseTreeCollectsFilesAndRequiresEmbeddedChildren(t *testing.T) {
	rootFile := referenceFor([]byte("root file"))
	childFile := referenceFor([]byte("child file"))
	child := bytesField(1, fileNodeProto(childFile))
	childReference := referenceFor(child)
	root := concatProto(
		bytesField(1, fileNodeProto(rootFile)),
		bytesField(2, directoryNodeProto(childReference)),
	)
	tree := concatProto(bytesField(1, root), bytesField(2, child))

	files, err := parseTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, files, rootFile, childFile)

	if _, err := parseTree(bytesField(1, root)); err == nil {
		t.Fatal("Tree with a missing embedded child was accepted")
	}
}

func TestActionResultReadRequiresCompleteCASClosure(t *testing.T) {
	output := []byte("cached output")
	outputReference := referenceFor(output)
	actionResult := bytesField(2, outputFileProto(outputReference, nil))
	actionDigest := strings.Repeat("a", 64)

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		backend.objects["test-v1-cas-"+outputReference.hash] = output
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), actionResult) {
			t.Fatalf("status/body = %d/%q", response.Code, response.Body.Bytes())
		}
		stats := server.Snapshot()
		if stats.ValidatedActionResults != 1 || stats.IncompleteActionResults != 0 ||
			stats.InvalidActionResults != 0 || stats.Hits != 1 || stats.BackendDownloads != 1 ||
			stats.BackendExistenceChecks != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
		stats := server.Snapshot()
		if stats.IncompleteActionResults != 1 || stats.Misses != 1 || stats.Hits != 0 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestActionResultReadAllowsImplicitEmptyCASDigest(t *testing.T) {
	emptyReference := referenceFor(nil)
	actionResult := bytesField(6, digestProto(emptyReference))
	actionDigest := strings.Repeat("1", 64)
	backend := newMemoryBackend()
	backend.objects["test-v1-ac-"+actionDigest] = actionResult
	server := testServer(t, backend, nil)

	response := readCacheObject(server, "/ac/"+actionDigest)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), actionResult) {
		t.Fatalf("status/body = %d/%q", response.Code, response.Body.Bytes())
	}
	stats := server.Snapshot()
	if stats.ValidatedActionResults != 1 || stats.IncompleteActionResults != 0 ||
		stats.Hits != 1 || stats.BackendDownloads != 1 || stats.BackendExistenceChecks != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestActionResultReadValidatesTreeFileClosure(t *testing.T) {
	fileReference := referenceFor([]byte("missing nested file"))
	tree := bytesField(1, bytesField(1, fileNodeProto(fileReference)))
	treeReference := referenceFor(tree)
	actionResult := bytesField(3, outputDirectoryProto(treeReference, digestReference{}))
	actionDigest := strings.Repeat("b", 64)

	backend := newMemoryBackend()
	backend.objects["test-v1-ac-"+actionDigest] = actionResult
	backend.objects["test-v1-cas-"+treeReference.hash] = tree
	server := testServer(t, backend, nil)

	response := readCacheObject(server, "/ac/"+actionDigest)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	stats := server.Snapshot()
	if stats.IncompleteActionResults != 1 || stats.BackendDownloads != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestActionResultReadValidatesRootDirectoryClosure(t *testing.T) {
	file := []byte("nested output")
	fileReference := referenceFor(file)
	child := bytesField(1, fileNodeProto(fileReference))
	childReference := referenceFor(child)
	root := bytesField(2, directoryNodeProto(childReference))
	rootReference := referenceFor(root)
	actionResult := bytesField(3, outputDirectoryProto(digestReference{}, rootReference))
	actionDigest := strings.Repeat("e", 64)

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		backend.objects["test-v1-cas-"+rootReference.hash] = root
		backend.objects["test-v1-cas-"+childReference.hash] = child
		backend.objects["test-v1-cas-"+fileReference.hash] = file
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.Code)
		}
		stats := server.Snapshot()
		if stats.ValidatedActionResults != 1 || stats.BackendDownloads != 3 ||
			stats.BackendExistenceChecks != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("missing nested file", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		backend.objects["test-v1-cas-"+rootReference.hash] = root
		backend.objects["test-v1-cas-"+childReference.hash] = child
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
		stats := server.Snapshot()
		if stats.IncompleteActionResults != 1 || stats.BackendDownloads != 3 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestActionResultReadRejectsInvalidPayload(t *testing.T) {
	actionDigest := strings.Repeat("c", 64)
	backend := newMemoryBackend()
	backend.objects["test-v1-ac-"+actionDigest] = []byte{0xff}
	server := testServer(t, backend, nil)

	response := readCacheObject(server, "/ac/"+actionDigest)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	stats := server.Snapshot()
	if stats.InvalidActionResults != 1 || stats.Misses != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestActionResultReadHandlesExistenceCheckErrors(t *testing.T) {
	outputReference := referenceFor([]byte("output"))
	actionResult := bytesField(2, outputFileProto(outputReference, nil))
	actionDigest := strings.Repeat("f", 64)

	for _, test := range []struct {
		name     string
		failOpen bool
		status   int
		misses   uint64
	}{
		{name: "fail open", failOpen: true, status: http.StatusNotFound, misses: 1},
		{name: "strict", failOpen: false, status: http.StatusBadGateway, misses: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newMemoryBackend()
			backend.objects["test-v1-ac-"+actionDigest] = actionResult
			backend.existsErr = errors.New("unavailable")
			server := testServer(t, backend, func(cfg *Config) {
				cfg.FailOpen = test.failOpen
			})

			response := readCacheObject(server, "/ac/"+actionDigest)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			stats := server.Snapshot()
			if stats.BackendLoadErrors != 1 || stats.Misses != test.misses ||
				stats.BackendExistenceChecks != 1 {
				t.Fatalf("unexpected stats: %+v", stats)
			}
		})
	}
}

func TestActionResultUploadPublishesOnlyCompleteCASClosure(t *testing.T) {
	output := []byte("persisted output")
	outputReference := referenceFor(output)
	actionResult := bytesField(2, outputFileProto(outputReference, nil))
	actionDigest := strings.Repeat("d", 64)

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-cas-"+outputReference.hash] = output
		server := testServer(t, backend, nil)

		response := putCacheObject(server, "/ac/"+actionDigest, actionResult)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		if !bytes.Equal(backend.objects["test-v1-ac-"+actionDigest], actionResult) {
			t.Fatal("complete action result was not published")
		}
		stats := server.Snapshot()
		if stats.Uploads != 1 || stats.ValidatedActionResults != 1 ||
			stats.SkippedActionResultUploads != 0 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		backend := newMemoryBackend()
		server := testServer(t, backend, nil)

		response := putCacheObject(server, "/ac/"+actionDigest, actionResult)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		if _, exists := backend.objects["test-v1-ac-"+actionDigest]; exists {
			t.Fatal("incomplete action result was published")
		}
		stats := server.Snapshot()
		if stats.Uploads != 0 || stats.IncompleteActionResults != 1 ||
			stats.SkippedActionResultUploads != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestActionResultUploadAllowsImplicitEmptyCASDigest(t *testing.T) {
	emptyReference := referenceFor(nil)
	actionResult := bytesField(8, digestProto(emptyReference))
	actionDigest := strings.Repeat("2", 64)
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)

	response := putCacheObject(server, "/ac/"+actionDigest, actionResult)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if !bytes.Equal(backend.objects["test-v1-ac-"+actionDigest], actionResult) {
		t.Fatal("action result with an implicit empty digest was not published")
	}
	stats := server.Snapshot()
	if stats.Uploads != 1 || stats.ValidatedActionResults != 1 ||
		stats.IncompleteActionResults != 0 || stats.SkippedActionResultUploads != 0 ||
		stats.BackendExistenceChecks != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func readCacheObject(server *Server, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func putCacheObject(server *Server, path string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func referenceFor(data []byte) digestReference {
	return digestReference{hash: digest(data), size: int64(len(data))}
}

func digestProto(reference digestReference) []byte {
	return concatProto(
		bytesField(1, []byte(reference.hash)),
		varintField(2, uint64(reference.size)),
	)
}

func outputFileProto(reference digestReference, inline []byte) []byte {
	message := bytesField(2, digestProto(reference))
	if inline != nil {
		message = append(message, bytesField(5, inline)...)
	}
	return message
}

func outputDirectoryProto(tree, root digestReference) []byte {
	var message []byte
	if tree.hash != "" {
		message = append(message, bytesField(3, digestProto(tree))...)
	}
	if root.hash != "" {
		message = append(message, bytesField(5, digestProto(root))...)
	}
	return message
}

func fileNodeProto(reference digestReference) []byte {
	return bytesField(2, digestProto(reference))
}

func directoryNodeProto(reference digestReference) []byte {
	return bytesField(2, digestProto(reference))
}

func bytesField(number protowire.Number, value []byte) []byte {
	message := protowire.AppendTag(nil, number, protowire.BytesType)
	return protowire.AppendBytes(message, value)
}

func varintField(number protowire.Number, value uint64) []byte {
	message := protowire.AppendTag(nil, number, protowire.VarintType)
	return protowire.AppendVarint(message, value)
}

func concatProto(fields ...[]byte) []byte {
	var message []byte
	for _, field := range fields {
		message = append(message, field...)
	}
	return message
}

func assertReferences(t *testing.T, got []digestReference, want ...digestReference) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("references = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("references[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
