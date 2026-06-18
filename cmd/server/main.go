// Command server is the backitup control plane: HTTP API + webgui + lifecycle
// timer in one process (design doc D1). The testable logic lives in
// internal/server; this is just wiring.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/th0rn0/backitup/internal/server"
	"github.com/th0rn0/backitup/internal/store"
)

func main() {
	dbPath := getenv("BACKITUP_DB", "/data/backitup.db")
	addr := getenv("BACKITUP_ADDR", ":8080")

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	log.Printf("backitup server: store ready at %s", dbPath)

	srv := server.New(st)
	log.Printf("backitup server: listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
