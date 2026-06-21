package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/config" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing/wrong bearer: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(ConfigResp{Mode: "rsync", Excludes: []string{"*.tmp"}, RetentionDays: 14})
	}))
	defer ts.Close()

	api, err := NewAPI(ts.URL, "tok", "", false)
	if err != nil {
		t.Fatalf("new api: %v", err)
	}
	cfg, err := api.FetchConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if cfg.Mode != "rsync" || cfg.RetentionDays != 14 || len(cfg.Excludes) != 1 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestFetchConfigNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()
	api, _ := NewAPI(ts.URL, "tok", "", false)
	if _, err := api.FetchConfig(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestPostStatus(t *testing.T) {
	var got StatusReq
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/status" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	api, _ := NewAPI(ts.URL, "tok", "", false)
	err := api.PostStatus(context.Background(), StatusReq{Status: "ok", Bytes: 99, SnapshotID: "s1"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if got.Status != "ok" || got.Bytes != 99 || got.SnapshotID != "s1" {
		t.Fatalf("server got %+v", got)
	}
}

func TestPostStatusNon201(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer ts.Close()
	api, _ := NewAPI(ts.URL, "tok", "", false)
	if err := api.PostStatus(context.Background(), StatusReq{Status: "ok"}); err == nil {
		t.Fatal("expected error on 400")
	}
}

func TestNewAPIBadCABundle(t *testing.T) {
	if _, err := NewAPI("https://x", "t", "/no/such/ca.pem", false); err == nil {
		t.Fatal("expected error for missing CA bundle")
	}
}
