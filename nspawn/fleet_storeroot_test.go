package nspawn

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testTop = "/nix/store/q9vxnwa7hnzbqddimsxnil0iyqcjk3yw-nixos-system-x"

// testImage builds a fake image with the NixOS profile link chain.
func testImage(t *testing.T, withInit bool) string {
	t.Helper()
	img := t.TempDir()
	if err := os.MkdirAll(filepath.Join(img, testTop), 0o755); err != nil {
		t.Fatal(err)
	}
	if withInit {
		if err := os.WriteFile(filepath.Join(img, testTop, "init"), []byte("#!/bin/sh\n"), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	prof := filepath.Join(img, "nix/var/nix/profiles")
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("system-1-link", filepath.Join(prof, "system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testTop, filepath.Join(prof, "system-1-link")); err != nil {
		t.Fatal(err)
	}
	return img
}

func TestFleetStoreRootIsStorePath(t *testing.T) {
	for p, want := range map[string]bool{
		testTop:                 true,
		"/nix/store/":           false,
		"/nix/store/..":         false,
		"/nix/store/a/init":     false,
		"/etc/nixos":            false,
		"nix/store/abc-x":       false,
		"/nix/store/abc-x/../y": false,
	} {
		if got := fleetIsStorePath(p); got != want {
			t.Errorf("%q: got %v", p, got)
		}
	}
}

func TestFleetStoreRootResolveProfile(t *testing.T) {
	img := testImage(t, true)
	got, err := fleetResolveToplevel(img, "")
	if err != nil || got != testTop {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err = fleetResolveToplevel(img, testTop); err != nil || got != testTop {
		t.Fatalf("explicit: got %q %v", got, err)
	}
}

func TestFleetStoreRootResolveNoInit(t *testing.T) {
	if _, err := fleetResolveToplevel(testImage(t, false), ""); err == nil {
		t.Fatal("want error without init")
	}
}

func TestFleetStoreRootResolveStaysInImage(t *testing.T) {
	// A link that leaves /nix/store must fail, not resolve on the host.
	img := testImage(t, true)
	prof := filepath.Join(img, "nix/var/nix/profiles")
	os.Remove(filepath.Join(prof, "system-1-link"))
	if err := os.Symlink("/etc", filepath.Join(prof, "system-1-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetResolveToplevel(img, ""); err == nil {
		t.Fatal("want error for a link outside the store")
	}
}

func TestFleetStoreRootResolveLoop(t *testing.T) {
	img := t.TempDir()
	prof := filepath.Join(img, "nix/var/nix/profiles")
	os.MkdirAll(prof, 0o755)
	os.Symlink("system", filepath.Join(prof, "system"))
	if _, err := fleetResolveToplevel(img, ""); err == nil {
		t.Fatal("want error for a link loop")
	}
}

func TestFleetStoreRootValidate(t *testing.T) {
	ok := []MachineConfig{
		{},
		{RootMode: "image"},
		{RootMode: "store"},
		{RootMode: "store", PrivateUsersOwnership: "map", RootSize: "512m", Toplevel: testTop},
	}
	for i, c := range ok {
		if err := fleetValidateStoreRoot(&c); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	bad := []MachineConfig{
		{RootMode: "overlay"},
		{Toplevel: testTop},
		{RootSize: "1g"},
		{RootMode: "store", Volatile: "overlay"},
		{RootMode: "store", Ephemeral: true},
		{RootMode: "store", ReadOnly: true},
		{RootMode: "store", Command: []string{"/bin/sh"}},
		{RootMode: "store", PrivateUsersOwnership: "auto"},
		{RootMode: "store", PrivateUsersOwnership: "chown"},
		{RootMode: "store", Toplevel: "/etc"},
		{RootMode: "store", RootSize: "0"},
		{RootMode: "store", RootSize: "1t"},
		{RootMode: "store", RootSize: "50%"},
	}
	for i, c := range bad {
		if err := fleetValidateStoreRoot(&c); err == nil {
			t.Errorf("bad %d accepted: %+v", i, c)
		}
	}
}

func TestFleetStoreRootConfigArray(t *testing.T) {
	img := testImage(t, true)
	c := MachineConfig{
		Image: img, imagePath: img, RootMode: "store", Boot: true,
		UserNamespacing: true, PrivateUsersOwnership: "map",
		NetworkNamespace: "/var/run/netns/x",
		storeRootDir:     "/alloc/t/nspawn-root", toplevelPath: testTop,
	}
	args, err := c.ConfigArray()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args[:2], []string{"-D", "/alloc/t/nspawn-root"}) {
		t.Fatalf("root: %v", args)
	}
	n := len(args)
	if args[n-1] != testTop+"/init" || args[n-2] != "--kill-signal=SIGRTMIN+3" ||
		args[n-3] != "--bind-ro="+img+"/nix/store:/nix/store:idmap" {
		t.Fatalf("tail: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, s := range []string{"--private-users=pick", "--private-users-ownership=map", "--network-namespace-path=/var/run/netns/x"} {
		if !strings.Contains(joined, s) {
			t.Errorf("missing %s in %v", s, args)
		}
	}
	if strings.Contains(joined, "--boot") || strings.Contains(joined, "--volatile") {
		t.Errorf("store mode must not boot or use volatile: %v", args)
	}
}

func TestFleetStoreRootConfigArrayNeedsPreparedRoot(t *testing.T) {
	img := testImage(t, true)
	c := MachineConfig{Image: img, imagePath: img, RootMode: "store"}
	if _, err := c.ConfigArray(); err == nil {
		t.Fatal("want error without a prepared root")
	}
}

func TestFleetStoreRootPortStartRange(t *testing.T) {
	if err := fleetSetUnprivilegedPortStart("/nonexistent", 70000); err == nil {
		t.Fatal("want range error")
	}
}
