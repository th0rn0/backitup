package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// API talks to the control channel (the app's HTTPS API) with a bearer token.
type API struct {
	base  string
	token string
	hc    *http.Client
}

// NewAPI builds an API client. caBundle (optional) trusts a self-signed server
// cert; insecure skips TLS verification (dev/test only).
func NewAPI(base, token, caBundle string, insecure bool) (*API, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case insecure:
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit dev opt-out
	case caBundle != "":
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certs in CA bundle %s", caBundle)
		}
		tlsCfg.RootCAs = pool
	}
	return &API{
		base:  base,
		token: token,
		hc: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// ConfigResp is the server's per-client config (the WHAT; design doc D1).
type ConfigResp struct {
	Mode          string   `json:"mode"`
	Excludes      []string `json:"excludes"`
	RetentionDays int      `json:"retention_days"`
}

// FetchConfig retrieves this client's config from the server.
func (a *API) FetchConfig(ctx context.Context) (ConfigResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/api/v1/config", nil)
	if err != nil {
		return ConfigResp{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.hc.Do(req)
	if err != nil {
		return ConfigResp{}, fmt.Errorf("fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ConfigResp{}, fmt.Errorf("fetch config: server returned %s", resp.Status)
	}
	var cfg ConfigResp
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return ConfigResp{}, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}

// StatusReq is the run result reported back to the server.
type StatusReq struct {
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	SnapshotID string    `json:"snapshot_id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	LogTail    string    `json:"log_tail"`
}

// PostStatus reports a run result.
func (a *API) PostStatus(ctx context.Context, s StatusReq) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/api/v1/status", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("post status: %w", err)
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("post status: server returned %s", resp.Status)
	}
	return nil
}
