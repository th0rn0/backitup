// Package alert sends notifications to external services.
package alert

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

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
