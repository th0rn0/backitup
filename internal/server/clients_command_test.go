package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// assertPasteSafe checks the invariant the generated docker command relies on:
// no line that ends in a line-continuation backslash may contain a '#' before
// it, because the shell starts a comment at the '#' and never sees the
// backslash. The breakage is silent in the rendered textarea and only shows up
// when the command is pasted:
//
//	docker: 'docker run' requires at least 1 argument
//	-e: command not found
func assertPasteSafe(t *testing.T, name, cmd string) {
	t.Helper()
	for i, line := range strings.Split(cmd, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, `\`) && strings.Contains(trimmed, "#") {
			t.Errorf("%s line %d has a '#' before its continuation backslash: %q", name, i+1, line)
		}
	}
}

// runWithFakeDocker writes cmd to a script, puts a fake `docker` first on PATH
// that records its argv, and runs the script with sh. It returns the argv the
// fake docker saw, so the test proves the command reaches docker intact rather
// than merely looking right.
func runWithFakeDocker(t *testing.T, cmd string) []string {
	t.Helper()
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argvFile + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	script := filepath.Join(dir, "cmd.sh")
	if err := os.WriteFile(script, []byte(cmd+"\n"), 0o755); err != nil {
		t.Fatalf("write command script: %v", err)
	}

	sh := exec.Command("sh", script)
	sh.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("generated command is not runnable: %v\n%s\n--- command ---\n%s", err, out, cmd)
	}
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake docker never ran: %v\n--- command ---\n%s", err, cmd)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestAPIBasePlaceholderIsBareURL(t *testing.T) {
	s := &Server{}
	if got := s.apiBase("http"); got != "http://YOUR-SERVER:8080" {
		t.Errorf("http placeholder = %q, want http://YOUR-SERVER:8080", got)
	}
	if got := s.apiBase("https"); got != "https://YOUR-SERVER:8080" {
		t.Errorf("https placeholder = %q, want https://YOUR-SERVER:8080", got)
	}
	if got := s.apiBase("gopher"); got != "http://YOUR-SERVER:8080" {
		t.Errorf("unknown scheme = %q, want the http placeholder", got)
	}
}

func TestDockerCmdsPlaceholderIsPasteSafe(t *testing.T) {
	s := &Server{publicHost: "backup.test:2222", clientImage: "th0rn0/backitup-client:dev"}
	apiBase := s.apiBase("http")
	known, insecure := s.dockerCmds("tok3n", apiBase, "/srv/photos", "/etc/backitup/nas", true)

	for name, cmd := range map[string]string{"known": known, "insecure": insecure} {
		assertPasteSafe(t, name, cmd)
		if strings.Contains(cmd, "#") {
			t.Errorf("%s carries a comment, so the crontab entry built from it would too:\n%s", name, cmd)
		}
		if !strings.Contains(cmd, "-e BACKITUP_API=http://YOUR-SERVER:8080 \\\n") {
			t.Errorf("%s does not carry a bare placeholder API value:\n%s", name, cmd)
		}

		args := runWithFakeDocker(t, cmd)
		if len(args) == 0 || args[0] != "run" {
			t.Fatalf("%s: docker argv = %q, want it to start with 'run'", name, args)
		}
		if got := lastArg(args); got != s.clientImage {
			t.Errorf("%s: last docker arg = %q, want image %q (argv=%q)", name, got, s.clientImage, args)
		}
		for _, want := range []string{
			"BACKITUP_API=http://YOUR-SERVER:8080",
			"BACKITUP_SERVER=backup.test:2222",
			"BACKITUP_TOKEN=tok3n",
			"BACKITUP_SSH_KEY=/secrets/id",
		} {
			if !containsArg(args, want) {
				t.Errorf("%s: docker never received %q (argv=%q)", name, want, args)
			}
		}
	}

	if !containsArg(runWithFakeDocker(t, known), "BACKITUP_KNOWN_HOSTS=/secrets/known_hosts") {
		t.Error("known variant is missing BACKITUP_KNOWN_HOSTS")
	}
	if !containsArg(runWithFakeDocker(t, insecure), "BACKITUP_INSECURE=1") {
		t.Error("insecure variant is missing BACKITUP_INSECURE=1")
	}
}

// TestDockerCmdsWithoutHostKeyHasNoVerifiedVariant is the half of the dead end
// that lives in Go: a verified command mounts a known_hosts file, so there is
// one only when the server can hand that file over.
func TestDockerCmdsWithoutHostKeyHasNoVerifiedVariant(t *testing.T) {
	s := &Server{publicHost: "backup.test:2222", clientImage: "img"}
	known, insecure := s.dockerCmds("tok3n", s.apiBase("http"), "/srv/photos", "/etc/backitup/nas", false)
	if known != "" {
		t.Errorf("no host key, yet a verified command was generated:\n%s", known)
	}
	if !strings.Contains(insecure, "-e BACKITUP_INSECURE=1 \\\n") {
		t.Errorf("insecure command does not say what it gives up:\n%s", insecure)
	}
	if strings.Contains(insecure, "BACKITUP_KNOWN_HOSTS") {
		t.Errorf("insecure command references a known_hosts file that does not exist:\n%s", insecure)
	}
}

func TestDockerCmdsConfiguredAPICarriesNoHint(t *testing.T) {
	s := &Server{publicHost: "backup.test:2222", publicAPI: "http://10.0.0.1:8080", clientImage: "img"}
	known, _ := s.dockerCmds("tok3n", s.apiBase("http"), "/src", "/sec", true)
	assertPasteSafe(t, "known", known)
	if strings.Contains(known, "#") {
		t.Errorf("configured command should carry no hint comment:\n%s", known)
	}
	if s.apiPlaceholderHint() != "" {
		t.Errorf("configured server still asks the operator to fix the API address: %q", s.apiPlaceholderHint())
	}
	if !strings.Contains(known, "-e BACKITUP_API=http://10.0.0.1:8080 \\\n") {
		t.Errorf("configured command does not use BACKITUP_PUBLIC_API:\n%s", known)
	}
	if !containsArg(runWithFakeDocker(t, known), "BACKITUP_API=http://10.0.0.1:8080") {
		t.Error("docker never received the configured BACKITUP_API value")
	}
}

// TestCronEntryIsACompleteCrontabLine is the unit-level guard for the fourth
// defect: the operator was handed a schedule and a comment where a runnable
// entry was promised. The entry must carry the schedule and the whole command,
// and the command part must reach docker intact once cron's continuation
// handling (a shell here) has joined the lines.
func TestCronEntryIsACompleteCrontabLine(t *testing.T) {
	s := &Server{publicHost: "backup.test:2222", clientImage: "img"}
	_, insecure := s.dockerCmds("tok3n", s.apiBase("http"), "/srv/photos", "/etc/backitup/nas", false)
	entry := cronEntry(defaultCronSchedule, insecure)

	if strings.Contains(entry, "#") {
		t.Errorf("cron entry carries a '#'; a crontab would read the rest of the line as a comment:\n%s", entry)
	}
	first, _, _ := strings.Cut(entry, "\n")
	if first != defaultCronSchedule+" docker run --rm \\" {
		t.Errorf("first line = %q, want the schedule followed by the command", first)
	}
	// The text a crontab holds is what runs, minus the schedule fields; prove the
	// command body is intact.
	args := runWithFakeDocker(t, strings.TrimPrefix(entry, defaultCronSchedule+" "))
	if !containsArg(args, "BACKITUP_TOKEN=tok3n") {
		t.Errorf("docker never received the token from the cron entry (argv=%q)", args)
	}
	if got := lastArg(args); got != s.clientImage {
		t.Errorf("last docker arg = %q, want the image %q", got, s.clientImage)
	}
}

// TestSecretsDirIsOneNameAndSourceIsNotBACKUP covers the two naming defects at
// the source: the directory named in step 1 and the directory mounted by the
// command come from the same function, and the source mount is only called a
// BACKUP when the operator's own label says so.
func TestSecretsDirIsOneNameAndSourceIsNotBACKUP(t *testing.T) {
	if got := secretsDir("nas-photos", ""); got != "/etc/backitup/nas-photos" {
		t.Errorf("default secrets dir = %q", got)
	}
	if got := secretsDir("nas-photos", " /srv/keys/nas "); got != "/srv/keys/nas" {
		t.Errorf("form secrets dir = %q, want the trimmed form value", got)
	}

	cases := []struct {
		form, label string
		want        string
		prefilled   bool
	}{
		{"", "/mnt/photos", "/mnt/photos", true},
		{"", "~/photos", "~/photos", true},
		{"", "photos on the NAS", sourcePlaceholder, false},
		{"", "", sourcePlaceholder, false},
		{"/srv/camera", "photos on the NAS", "/srv/camera", true},
	}
	for _, tc := range cases {
		got, prefilled := sourceMount(tc.form, tc.label)
		if got != tc.want || prefilled != tc.prefilled {
			t.Errorf("sourceMount(%q, %q) = (%q, %v), want (%q, %v)",
				tc.form, tc.label, got, prefilled, tc.want, tc.prefilled)
		}
		if strings.Contains(got, "BACKUP") {
			t.Errorf("source mount %q names the source BACKUP", got)
		}
	}
}

// TestComposePassesPublicAPIToApp guards the other half of the defect: the
// repo compose file must forward BACKITUP_PUBLIC_API to the app container, or
// every compose user gets the placeholder command even after setting it in
// their env/.env, as the README Quick-start tells them to.
func TestComposePassesPublicAPIToApp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	body := string(data)
	start := strings.Index(body, "\n  app:\n")
	end := strings.Index(body, "\n  sshd:\n")
	if start < 0 || end < start {
		t.Fatalf("could not isolate the app service in docker-compose.yml")
	}
	appBlock := body[start:end]
	if !strings.Contains(appBlock, "BACKITUP_PUBLIC_API:") {
		t.Error("docker-compose.yml does not pass BACKITUP_PUBLIC_API to the app service; " +
			"compose users would silently get the placeholder client command")
	}
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
