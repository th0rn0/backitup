package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The fidelity settings reach the client through /api/v1/config and must be
// forwarded to the mode, and a server that predates them (keys absent) must
// decode as "every setting off" — otherwise an old server plus a new client
// would start sending flags the old receiver may refuse.
func TestFetchConfigCarriesFidelitySettings(t *testing.T) {
	t.Run("keys present", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mode": "rsync", "preserve_sparse": true, "preserve_hardlinks": true,
				"preserve_acls": true, "preserve_xattrs": true,
			})
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
		if !cfg.PreserveSparse || !cfg.PreserveHardlinks || !cfg.PreserveACLs || !cfg.PreserveXattrs {
			t.Fatalf("the fidelity settings did not decode: %+v", cfg)
		}
		if got, want := fidelityNames(cfg), []string{"sparse", "hardlinks", "acls", "xattrs"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fidelityNames = %v, want %v", got, want)
		}
	})

	t.Run("keys absent", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "rsync"})
		}))
		defer ts.Close()
		api, _ := NewAPI(ts.URL, "tok", "", false)
		cfg, err := api.FetchConfig(context.Background())
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if cfg.PreserveSparse || cfg.PreserveHardlinks || cfg.PreserveACLs || cfg.PreserveXattrs {
			t.Fatalf("a config without the keys decoded with a setting on: %+v", cfg)
		}
		if got := fidelityNames(cfg); len(got) != 0 {
			t.Errorf("fidelityNames = %v, want none (and not nil, so the log says [])", got)
		} else if got == nil {
			t.Error("fidelityNames returned nil; the run log would print <nil>")
		}
	})
}
