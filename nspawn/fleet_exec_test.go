package nspawn

import (
	"reflect"
	"strings"
	"testing"
)

func TestFleetExecEnvDefaults(t *testing.T) {
	got := fleetExecEnv(nil, false)
	want := []string{"PATH=" + fleetDefaultExecPath, "HOME=/root", "USER=root", "LOGNAME=root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(fleetDefaultExecPath, "/usr/bin") || !strings.Contains(fleetDefaultExecPath, "/bin") {
		t.Fatal("classic paths missing")
	}
}

func TestFleetExecEnvTaskOverrideAndTerm(t *testing.T) {
	got := fleetExecEnv(map[string]string{"PATH": "/x", "TERM": "vt100"}, true)
	want := []string{"PATH=/x", "HOME=/root", "USER=root", "LOGNAME=root", "TERM=vt100"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got = fleetExecEnv(nil, true); got[len(got)-1] != "TERM=xterm-256color" {
		t.Fatalf("got %v", got)
	}
}

func TestFleetExecEnvCommandOrder(t *testing.T) {
	got := fleetExecCommand("m", []string{"ls", "-l"}, false, nil)
	if got[0] != "systemd-run" || got[6] != "m" || got[7] != "--pipe" {
		t.Fatalf("bad prefix %v", got)
	}
	n := len(got)
	if got[n-2] != "ls" || got[n-1] != "-l" || !strings.HasPrefix(got[n-3], "--setenv=") {
		t.Fatalf("bad order %v", got)
	}
	pty := fleetExecCommand("m", []string{"sh"}, true, nil)
	if pty[7] != "--pty" || pty[8] != "--send-sighup" {
		t.Fatalf("bad pty %v", pty)
	}
}
