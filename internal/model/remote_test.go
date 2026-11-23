package model

import (
	"strings"
	"testing"
)

// A crypt remote's section must point rclone at the plaintext remote it
// encrypts: `<parent>:` is the key material that decides whether the provider
// sees ciphertext or the backup itself.
func TestCryptSectionReferencesPlaintextRemote(t *testing.T) {
	r := Remote{
		Name:    "vault",
		Backend: BackendCrypt,
		Config: map[string]string{
			"remote":                    "provider",
			"directory_name_encryption": "true",
			"filename_encryption":       "standard",
			"password":                  "obscured-password",
			"password2":                 "obscured-salt",
		},
	}
	got := r.RcloneSection()
	for _, want := range []string{
		"[vault]",
		"type = crypt",
		"remote = provider:",
		"directory_name_encryption = true",
		"filename_encryption = standard",
		"password = obscured-password",
		"password2 = obscured-salt",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("crypt section missing %q:\n%s", want, got)
		}
	}
}

func TestCryptParent(t *testing.T) {
	cases := []struct {
		name   string
		remote Remote
		want   string
	}{
		{"plain name", Remote{Backend: BackendCrypt, Config: map[string]string{"remote": "provider"}}, "provider"},
		{"rclone syntax", Remote{Backend: BackendCrypt, Config: map[string]string{"remote": "provider:"}}, "provider"},
		{"padded", Remote{Backend: BackendCrypt, Config: map[string]string{"remote": "  provider  "}}, "provider"},
		{"unset", Remote{Backend: BackendCrypt}, ""},
		{"not a crypt remote", Remote{Backend: BackendB2, Config: map[string]string{"remote": "provider"}}, ""},
	}
	for _, tc := range cases {
		if got := tc.remote.CryptParent(); got != tc.want {
			t.Errorf("%s: CryptParent() = %q, want %q", tc.name, got, tc.want)
		}
	}
	if !(Remote{Backend: BackendCrypt}).IsCrypt() {
		t.Error("IsCrypt() = false for the crypt backend")
	}
	if (Remote{Backend: BackendB2}).IsCrypt() {
		t.Error("IsCrypt() = true for a plaintext backend")
	}
}

// The form offers checks only for what it renders; the handler asks the field
// definition, so the vocabulary has to live there.
func TestAllowsValue(t *testing.T) {
	sel := RemoteField{Type: FieldSelect, Options: []SelectOpt{{Value: "standard"}, {Value: "off"}}}
	if !sel.AllowsValue("standard") || !sel.AllowsValue("off") {
		t.Error("a declared option was rejected")
	}
	if sel.AllowsValue("rot13") || sel.AllowsValue("") {
		t.Error("an undeclared option was accepted")
	}
	text := RemoteField{Type: FieldText, Options: []SelectOpt{{Value: "only"}}}
	if !text.AllowsValue("anything") {
		t.Error("a text field must accept any value")
	}
}

// Every backend in the list must be resolvable and carry fields — a typo in an
// ID silently removes the backend from the form instead of failing to build.
func TestBackendsAreWellFormed(t *testing.T) {
	seen := map[RemoteBackend]bool{}
	for _, def := range Backends {
		if seen[def.ID] {
			t.Errorf("backend %q is listed twice", def.ID)
		}
		seen[def.ID] = true
		if FindBackend(def.ID) == nil {
			t.Errorf("backend %q is not resolvable by FindBackend", def.ID)
		}
		if def.Label == "" {
			t.Errorf("backend %q has no label", def.ID)
		}
		if len(def.Fields) == 0 {
			t.Errorf("backend %q has no fields", def.ID)
		}
		for _, f := range def.Fields {
			if f.Key == "" || f.Label == "" {
				t.Errorf("backend %q has a field without a key or label: %+v", def.ID, f)
			}
			if f.Type == FieldSelect {
				if len(f.Options) == 0 {
					t.Errorf("backend %q field %q is a select with no options", def.ID, f.Key)
				}
				if f.Default != "" && !f.AllowsValue(f.Default) {
					t.Errorf("backend %q field %q defaults to %q, which is not one of its options", def.ID, f.Key, f.Default)
				}
			}
		}
	}
	if !seen[BackendCrypt] {
		t.Error("the crypt backend is not offered")
	}
}

// The section must be byte-identical on every call: the fields now live inside
// a file we promise to preserve around, so a section that reshuffled itself on
// each write would look like an edit to the operator (Go map order used to
// decide the field order).
func TestRcloneSectionIsDeterministic(t *testing.T) {
	r := Remote{
		Name:    "vault",
		Backend: BackendCrypt,
		Config: map[string]string{
			"remote":                    "provider",
			"filename_encryption":       "standard",
			"directory_name_encryption": "true",
			"password":                  "obscured-password",
			"password2":                 "obscured-salt",
		},
	}
	first := r.RcloneSection()
	for i := 0; i < 25; i++ {
		if got := r.RcloneSection(); got != first {
			t.Fatalf("RcloneSection() changed between calls:\nfirst:\n%s\nlater:\n%s", first, got)
		}
	}
	// Sorted, so `password` precedes `password2` precedes `remote`.
	pw, pw2, rem := strings.Index(first, "password = "), strings.Index(first, "password2 = "), strings.Index(first, "remote = ")
	if pw < 0 || pw2 < 0 || rem < 0 || pw > pw2 || pw2 > rem {
		t.Fatalf("config keys are not in a stable sorted order:\n%s", first)
	}
}

// Config values are written one per line; a value carrying a newline must be
// flattened, or it could introduce a section of its own.
func TestRcloneSectionFlattensNewlinesInValues(t *testing.T) {
	r := Remote{
		Name:    "vault",
		Backend: BackendCrypt,
		Config: map[string]string{
			"remote":   "provider",
			"password": "a\n[evil]\ntype = s3",
		},
	}
	got := r.RcloneSection()
	sections := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			sections++
		}
	}
	if sections != 1 {
		t.Fatalf("a value injected a config section:\n%s", got)
	}
}

// A lone \r is a line break to some INI readers (rclone 1.76 treats it as part
// of the value, but the file is read by more than rclone): it must not survive
// into rclone.conf either.
func TestRcloneSectionFlattensCarriageReturnsInValues(t *testing.T) {
	r := Remote{
		Name:    "vault",
		Backend: BackendCrypt,
		Config: map[string]string{
			"remote":   "provider",
			"password": "a\r[evil]\rtype = s3",
		},
	}
	got := r.RcloneSection()
	if strings.ContainsRune(got, '\r') {
		t.Fatalf("a carriage return survived into the section:\n%q", got)
	}
	sections := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") && strings.HasSuffix(line, "]") {
			sections++
		}
	}
	if sections != 1 {
		t.Fatalf("a value injected a config section:\n%q", got)
	}
	if !strings.Contains(got, "password = a [evil] type = s3") {
		t.Fatalf("the value was not flattened onto one line:\n%q", got)
	}
}
