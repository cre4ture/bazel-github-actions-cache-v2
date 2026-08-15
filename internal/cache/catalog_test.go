package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestActionsCatalogListsAllPagesWithinPrefix(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.URL.Query().Get("key"); got != "prefix-manifest-" {
			t.Fatalf("key = %q", got)
		}
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"actions_caches":[{"key":"prefix-manifest-one"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"actions_caches":[]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &ActionsCatalog{baseURL: baseURL, repository: "owner/repository", token: "token", client: server.Client()}
	keys, err := catalog.List(context.Background(), "prefix-manifest-", 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "prefix-manifest-one" || requests != 1 {
		t.Fatalf("keys/requests = %v/%d", keys, requests)
	}
}

func TestActionsCatalogRejectsLimitOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"actions_caches":[{"key":"prefix-one"},{"key":"prefix-two"}]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &ActionsCatalog{baseURL: baseURL, repository: "owner/repository", token: "token", client: server.Client()}
	if _, err := catalog.List(context.Background(), "prefix-", 1); err == nil {
		t.Fatal("overflow was accepted")
	}
}
