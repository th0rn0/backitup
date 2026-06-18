package keys

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateKeypair(t *testing.T) {
	priv, pub, err := GenerateKeypair("backitup:laptop")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(priv, "OPENSSH PRIVATE KEY") {
		t.Fatalf("private key not in OpenSSH PEM form: %q", priv[:40])
	}
	if _, err := ssh.ParsePrivateKey([]byte(priv)); err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	// Public line must be a single, valid authorized_keys entry with the comment.
	if strings.ContainsAny(pub, "\n\r") {
		t.Fatal("public line must be single-line")
	}
	if !strings.HasSuffix(pub, "backitup:laptop") {
		t.Fatalf("comment missing from public line: %q", pub)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub)); err != nil {
		t.Fatalf("public line not a valid authorized_keys entry: %v", err)
	}
}

func TestKeypairsDiffer(t *testing.T) {
	_, a, _ := GenerateKeypair("x")
	_, b, _ := GenerateKeypair("x")
	if a == b {
		t.Fatal("two generated public keys are identical")
	}
}

func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	b, _ := GenerateToken()
	if a == "" || a == b {
		t.Fatalf("tokens should be non-empty and unique: %q vs %q", a, b)
	}
	if strings.ContainsAny(a, "+/=\n") {
		t.Fatalf("token should be URL-safe base64 without padding: %q", a)
	}
}
