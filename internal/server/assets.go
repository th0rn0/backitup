package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// serveAsset serves the embedded static files with a validator and a
// revalidate-always cache policy.
//
// Without them the browser applies heuristic freshness to /static/app.css and
// /static/*.js, so after a deploy it can keep running the previous build's
// scripts against the new templates. The symptom is a dead-looking UI: buttons
// that call handlers the old script never bound. Revalidation costs one
// conditional request per asset and makes that impossible.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(mustSub(assetFS), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", contentTypeFor(name))
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// contentTypeFor is the small extension map the app ships; anything unknown is
// served as octet-stream, which is safe under nosniff.
func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	}
	return "application/octet-stream"
}
