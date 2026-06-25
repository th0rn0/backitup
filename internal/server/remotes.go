package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type rcloneRemote struct {
	Name string
	Type string
}

// pendingGDriveAuth holds state for an in-progress Google Drive OAuth flow.
type pendingGDriveAuth struct {
	remoteName   string
	clientID     string
	clientSecret string
	expiresAt    time.Time
}

// listRcloneRemotes returns all remotes in the rclone config, sorted by name.
func (s *Server) listRcloneRemotes(ctx context.Context) ([]rcloneRemote, error) {
	if s.rcloneConfig == "" {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "config", "dump").Output()
	if err != nil {
		return nil, fmt.Errorf("rclone config dump: %w", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 || strings.TrimSpace(string(out)) == "null" {
		return nil, nil
	}
	var dump map[string]map[string]string
	if err := json.Unmarshal(out, &dump); err != nil {
		return nil, fmt.Errorf("parse rclone config: %w", err)
	}
	remotes := make([]rcloneRemote, 0, len(dump))
	for name, cfg := range dump {
		remotes = append(remotes, rcloneRemote{Name: name, Type: cfg["type"]})
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return remotes, nil
}

// getRemotes renders the remote storage management page.
func (s *Server) getRemotes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	remotes, listErr := s.listRcloneRemotes(ctx)
	errMsg := r.URL.Query().Get("err")
	flash := r.URL.Query().Get("msg")
	if listErr != nil && errMsg == "" {
		errMsg = "Could not list remotes: " + listErr.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "remotes.html", map[string]any{
		"Username":  usernameFromContext(r.Context()),
		"Remotes":   remotes,
		"Flash":     flash,
		"Error":     errMsg,
		"NoRclone":  s.rcloneConfig == "",
		"PublicAPI": s.publicAPI,
	})
}

// postCreateS3Remote creates an S3-compatible rclone remote non-interactively.
func (s *Server) postCreateS3Remote(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/remotes?err=invalid+form", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	accessKeyID := strings.TrimSpace(r.PostFormValue("access_key_id"))
	secretKey := strings.TrimSpace(r.PostFormValue("secret_access_key"))
	region := strings.TrimSpace(r.PostFormValue("region"))
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))

	if name == "" || accessKeyID == "" || secretKey == "" {
		http.Redirect(w, r, "/settings/remotes?err=Name%2C+access+key%2C+and+secret+are+required", http.StatusSeeOther)
		return
	}
	if !validRemoteName(name) {
		http.Redirect(w, r, "/settings/remotes?err=Remote+name+may+only+contain+letters%2C+digits%2C+hyphens%2C+underscores%2C+and+dots", http.StatusSeeOther)
		return
	}

	// Use provider=Other when a custom endpoint is given so rclone doesn't
	// apply AWS-specific defaults that would conflict with other providers.
	provider := "AWS"
	if endpoint != "" {
		provider = "Other"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	args := []string{"--config", s.rcloneConfig, "config", "create", name, "s3",
		"provider=" + provider,
		"access_key_id=" + accessKeyID,
		"secret_access_key=" + secretKey,
	}
	if region != "" {
		args = append(args, "region="+region)
	}
	if endpoint != "" {
		args = append(args, "endpoint="+endpoint)
	}

	if out, err := exec.CommandContext(ctx, "rclone", args...).CombinedOutput(); err != nil {
		log.Printf("rclone config create s3 %q: %v: %s", name, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("rclone error: "+strings.TrimSpace(string(out))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/settings/remotes?msg=Remote+%22"+url.QueryEscape(name)+"%22+created", http.StatusSeeOther)
}

// postStartGDriveAuth kicks off the Google Drive OAuth 2.0 authorization code
// flow. It stores a short-lived session keyed by a random state token, then
// redirects the user to Google's consent screen.
func (s *Server) postStartGDriveAuth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/remotes?err=invalid+form", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	clientID := strings.TrimSpace(r.PostFormValue("client_id"))
	clientSecret := strings.TrimSpace(r.PostFormValue("client_secret"))

	if name == "" || clientID == "" || clientSecret == "" {
		http.Redirect(w, r, "/settings/remotes?err=All+fields+are+required", http.StatusSeeOther)
		return
	}
	if !validRemoteName(name) {
		http.Redirect(w, r, "/settings/remotes?err=Remote+name+may+only+contain+letters%2C+digits%2C+hyphens%2C+underscores%2C+and+dots", http.StatusSeeOther)
		return
	}
	if s.publicAPI == "" {
		http.Redirect(w, r, "/settings/remotes?err=BACKITUP_PUBLIC_API+must+be+set+to+use+Google+Drive+OAuth", http.StatusSeeOther)
		return
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(b)

	s.oauthMu.Lock()
	for k, v := range s.oauthSessions { // sweep expired sessions
		if time.Now().After(v.expiresAt) {
			delete(s.oauthSessions, k)
		}
	}
	s.oauthSessions[state] = &pendingGDriveAuth{
		remoteName:   name,
		clientID:     clientID,
		clientSecret: clientSecret,
		expiresAt:    time.Now().Add(10 * time.Minute),
	}
	s.oauthMu.Unlock()

	params := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {s.publicAPI + "/oauth/gdrive/callback"},
		"response_type": {"code"},
		"scope":         {"https://www.googleapis.com/auth/drive"},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+params.Encode(), http.StatusSeeOther)
}

// getGDriveCallback handles Google's OAuth redirect. It is intentionally NOT
// behind requireAdmin — Google redirects the user's browser here, so the route
// must be publicly reachable. Security comes from the random state token.
func (s *Server) getGDriveCallback(w http.ResponseWriter, r *http.Request) {
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Google denied access: "+errParam), http.StatusSeeOther)
		return
	}

	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	s.oauthMu.Lock()
	session, ok := s.oauthSessions[state]
	if ok {
		delete(s.oauthSessions, state)
	}
	s.oauthMu.Unlock()

	if !ok || time.Now().After(session.expiresAt) {
		http.Redirect(w, r, "/settings/remotes?err=OAuth+session+expired+or+invalid%2C+please+try+again", http.StatusSeeOther)
		return
	}

	callbackURL := s.publicAPI + "/oauth/gdrive/callback"
	tok, err := exchangeGoogleCode(r.Context(), code, session.clientID, session.clientSecret, callbackURL)
	if err != nil {
		log.Printf("gdrive oauth exchange for %q: %v", session.remoteName, err)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Token exchange failed: "+err.Error()), http.StatusSeeOther)
		return
	}

	expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	tokenJSON, _ := json.Marshal(map[string]any{
		"access_token":  tok.AccessToken,
		"token_type":    tok.TokenType,
		"refresh_token": tok.RefreshToken,
		"expiry":        expiry.Format(time.RFC3339),
	})

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "config", "create",
		session.remoteName, "drive",
		"client_id="+session.clientID,
		"client_secret="+session.clientSecret,
		"token="+string(tokenJSON),
		"scope=drive",
	).CombinedOutput()
	if err != nil {
		log.Printf("rclone config create drive %q: %v: %s", session.remoteName, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("rclone config create failed: "+strings.TrimSpace(string(out))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/settings/remotes?msg=Google+Drive+remote+%22"+url.QueryEscape(session.remoteName)+"%22+connected", http.StatusSeeOther)
}

type googleTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func exchangeGoogleCode(ctx context.Context, code, clientID, clientSecret, redirectURI string) (*googleTokenResponse, error) {
	params := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token",
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok googleTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	return &tok, nil
}

// postDeleteRemote removes a remote from the rclone config by name.
func (s *Server) postDeleteRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRemoteName(name) {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "config", "delete", name).CombinedOutput(); err != nil {
		log.Printf("rclone config delete %q: %v: %s", name, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("delete failed: "+strings.TrimSpace(string(out))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/settings/remotes?msg=Remote+%22"+url.QueryEscape(name)+"%22+deleted", http.StatusSeeOther)
}

// validRemoteName accepts the character set rclone allows for remote names.
func validRemoteName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
			(c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
