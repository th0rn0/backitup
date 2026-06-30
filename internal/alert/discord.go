// Package alert sends notifications to external services.
package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// FormatBytes formats a byte count as a human-readable string (KB/MB/GB).
func FormatBytes(b int64) string {
	const (
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/MB)
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
}

// Discord posts content to a Discord webhook URL.
// If webhookURL is empty, it is a no-op.
func Discord(webhookURL, content string) {
	if webhookURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"content": content})
	resp, err := httpClient.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("alert: discord: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("alert: discord: unexpected status %d", resp.StatusCode)
	}
}
