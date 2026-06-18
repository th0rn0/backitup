// Package keys generates the per-client credentials backitup issues at client
// creation (design doc D4): an SSH keypair (data channel) and a bearer token
// (control channel). The private key and token are shown to the admin ONCE and
// never stored; only the public key and a hash of the token are persisted.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateKeypair returns a new ed25519 SSH keypair: the private key in OpenSSH
// PEM form (to hand to the client once) and the public key as a single
// authorized_keys line (to store and write into authorized_keys). comment is
// embedded in both for identification, e.g. "backitup:laptop-docs".
func GenerateKeypair(comment string) (privPEM string, pubLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("ssh public key: %w", err)
	}
	// MarshalAuthorizedKey returns "ssh-ed25519 AAAA...\n"; append the comment.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return string(pem.EncodeToMemory(block)), line, nil
}

// GenerateToken returns a 256-bit URL-safe random bearer token.
func GenerateToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
