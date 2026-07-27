package cache

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewActionsBackendRequiresV2Environment(t *testing.T) {
	t.Setenv("ACTIONS_CACHE_SERVICE_V2", "false")
	t.Setenv("ACTIONS_RESULTS_URL", "https://example.invalid/")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "not-a-token")
	_, err := NewActionsBackend(time.Second)
	if err == nil || !strings.Contains(err.Error(), "legacy cache v1") {
		t.Fatalf("error = %v", err)
	}
}

func TestFileBlobDoesNotOwnFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "blob-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	blob := fileBlob{file: file, size: 7}
	if blob.Size() != 7 {
		t.Fatalf("size = %d", blob.Size())
	}
	if err := blob.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("still open"); err != nil {
		t.Fatalf("blob closed caller-owned file: %v", err)
	}
}
