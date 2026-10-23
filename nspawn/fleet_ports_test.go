package nspawn

import (
	"strings"
	"testing"
)

func TestFleetPortMapFromEnv(t *testing.T) {
	env := map[string]string{"NOMAD_HOST_PORT_ssh": "24001", "NOMAD_HOST_PORT_web_ui": "24002"}
	got, err := fleetPortMapFromEnv(map[string]int{"ssh": 22, "web-ui": 8080}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got["ssh"] != "24001:22" || got["web-ui"] != "24002:8080" || len(got) != 2 {
		t.Fatalf("unexpected mapping %v", got)
	}
}

func TestFleetPortMapMissingLabels(t *testing.T) {
	env := map[string]string{"NOMAD_HOST_PORT_ok": "1000", "NOMAD_HOST_PORT_bad": "x"}
	_, err := fleetPortMapFromEnv(map[string]int{"ok": 1, "bad": 2, "gone": 3}, env)
	if err == nil || !strings.Contains(err.Error(), "bad, gone") {
		t.Fatalf("expected error naming bad, gone: %v", err)
	}
}

func TestFleetPortMapSkipsPortArgsInNetns(t *testing.T) {
	has := func(args []string) bool {
		for _, a := range args {
			if a == "-p" {
				return true
			}
		}
		return false
	}
	var c MachineConfig
	c.Port = map[string]string{"ssh": "1:22"}
	c.Image = "x"
	c.imagePath = t.TempDir()
	args, err := c.ConfigArray()
	if err != nil {
		t.Fatal(err)
	}
	if !has(args) {
		t.Fatalf("expected -p without netns: %v", args)
	}
	c.NetworkNamespace = "/var/run/netns/x"
	if args, err = c.ConfigArray(); err != nil || has(args) {
		t.Fatalf("unexpected -p with netns: %v %v", args, err)
	}
}
