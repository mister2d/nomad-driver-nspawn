package nspawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetCredentialsID(t *testing.T) {
	good := []string{"a", "ssh.authorized_keys.root", "my-cred_1", strings.Repeat("a", 255)}
	bad := []string{"", ".hidden", "a/b", "a:b", "a b", "ü", strings.Repeat("a", 256)}
	for _, id := range good {
		if err := fleetValidateCredentialID(id); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
	for _, id := range bad {
		if err := fleetValidateCredentialID(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

func fleetCredFixture(t *testing.T) (task string, roots []string) {
	t.Helper()
	task = t.TempDir()
	local := filepath.Join(task, "local")
	secrets := filepath.Join(task, "secrets")
	for _, d := range []string{local, secrets} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(secrets, "key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "cfg"), []byte("c"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task, "outside"), []byte("o"), 0o600); err != nil {
		t.Fatal(err)
	}
	return task, []string{local, secrets}
}

func TestFleetCredentialsPathResolution(t *testing.T) {
	task, roots := fleetCredFixture(t)
	real, _ := filepath.EvalSymlinks(task)
	got, err := fleetResolveCredentialPath(task, roots, "secrets/key")
	if err != nil || got != filepath.Join(real, "secrets", "key") {
		t.Fatalf("relative: %q %v", got, err)
	}
	if _, err := fleetResolveCredentialPath(task, roots, filepath.Join(task, "local", "cfg")); err != nil {
		t.Fatalf("absolute inside: %v", err)
	}
	if _, err := fleetResolveCredentialPath(task, roots, "local/../secrets/key"); err != nil {
		t.Fatalf("clean inside: %v", err)
	}
}

func TestFleetCredentialsPathRefusals(t *testing.T) {
	task, roots := fleetCredFixture(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "x"), filepath.Join(roots[1], "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(roots[0], "dirlink")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"empty":           "",
		"dotdot":          "secrets/../outside",
		"dotdot deep":     "../../etc/passwd",
		"abs outside":     filepath.Join(other, "x"),
		"file symlink":    "secrets/link",
		"dir symlink":     "local/dirlink/x",
		"colon":           "secrets/a:b",
		"task dir itself": ".",
		"directory":       "secrets",
		"missing":         "secrets/nope",
		"task root file":  "outside",
	}
	for name, p := range cases {
		if _, err := fleetResolveCredentialPath(task, roots, p); err == nil {
			t.Errorf("%s: %q accepted", name, p)
		}
	}
}

func TestFleetCredentialsArgsSortedAndRejected(t *testing.T) {
	task, roots := fleetCredFixture(t)
	real, _ := filepath.EvalSymlinks(task)
	args, err := fleetLoadCredentialArgs(task, roots, map[string]string{
		"zeta":  "local/cfg",
		"alpha": "secrets/key",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--load-credential=alpha:" + filepath.Join(real, "secrets", "key"),
		"--load-credential=zeta:" + filepath.Join(real, "local", "cfg"),
	}
	if len(args) != 2 || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("got %v want %v", args, want)
	}
	if _, err := fleetLoadCredentialArgs(task, roots, map[string]string{".bad": "local/cfg"}); err == nil {
		t.Error("bad ID accepted")
	}
	if _, err := fleetLoadCredentialArgs(task, roots, map[string]string{"ok": "../x"}); err == nil {
		t.Error("escape accepted")
	}
	if a, err := fleetLoadCredentialArgs(task, roots, nil); err != nil || len(a) != 0 {
		t.Errorf("empty: %v %v", a, err)
	}
}

func TestFleetCredentialsRenderedInArgs(t *testing.T) {
	c := &MachineConfig{Image: "x", imagePath: t.TempDir(), loadCredentialArgs: []string{"--load-credential=a:/x"}}
	args, err := c.ConfigArray()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range args {
		if a == "--load-credential=a:/x" {
			found = true
		}
	}
	if !found {
		t.Error("credential argument missing")
	}
}

func TestFleetCredentialsSetArgs(t *testing.T) {
	args, err := fleetSetCredentialArgs(map[string]string{"motd": "hi:there", "agent.name": "a1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--set-credential=agent.name:a1" || args[1] != "--set-credential=motd:hi:there" {
		t.Fatalf("got %v", args)
	}
	for _, id := range []string{"db.password", "API_TOKEN", "ssh.authorized_keys.root", "tls.cert", ".x", ""} {
		if _, err := fleetSetCredentialArgs(map[string]string{id: "v"}, nil); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
	if _, err := fleetSetCredentialArgs(map[string]string{"dup": "v"}, map[string]string{"dup": "secrets/x"}); err == nil {
		t.Error("duplicate ID accepted")
	}
}

func TestFleetCredentialsRedaction(t *testing.T) {
	in := []string{"-D", "/img", "--set-credential=motd:s3cret:x", "--load-credential=a:/p"}
	out := fleetRedactArgs(in)
	if strings.Contains(strings.Join(out, " "), "s3cret") {
		t.Fatalf("value leaked: %v", out)
	}
	if out[2] != "--set-credential=motd:<redacted>" || out[3] != in[3] {
		t.Fatalf("got %v", out)
	}
	if in[2] != "--set-credential=motd:s3cret:x" {
		t.Error("input modified")
	}
}
