package nspawn

import (
	"reflect"
	"sort"
	"testing"
)

func TestFleetEnvName(t *testing.T) {
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"NOMAD_ALLOC_ID", "NOMAD_ALLOC_ID", false},
		{"NOMAD_ALLOC_IP_connect-proxy-web", "NOMAD_ALLOC_IP_connect_proxy_web", true},
		{"NOMAD_UPSTREAM_ADDR_prometheus", "NOMAD_UPSTREAM_ADDR_prometheus", false},
		{"9LIVES", "_9LIVES", true},
		{"a.b", "a_b", true},
	}
	for _, c := range cases {
		got, ch := fleetEnvName(c.in)
		if got != c.want || ch != c.changed {
			t.Errorf("fleetEnvName(%q) = %q, %v; want %q, %v", c.in, got, ch, c.want, c.changed)
		}
	}
}

func TestFleetMergeTaskEnv(t *testing.T) {
	dst := map[string]string{"FROM_JOB": "1", "X_Y": "job"}
	skipped := fleetMergeTaskEnv(dst, map[string]string{
		"NOMAD_ALLOC_IP_connect-proxy-web": "172.26.64.68",
		"X-Y":                              "nomad", // X_Y is taken by the job: skip
		"A_B":                              "real",
		"A-B":                              "renamed", // collides with A_B from Nomad: skip
		"PLAIN":                            "p",
	})
	want := map[string]string{"FROM_JOB": "1", "X_Y": "job", "NOMAD_ALLOC_IP_connect_proxy_web": "172.26.64.68",
		"A_B": "real", "PLAIN": "p"}
	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("dst = %v", dst)
	}
	sort.Strings(skipped)
	if !reflect.DeepEqual(skipped, []string{"A-B", "X-Y"}) {
		t.Fatalf("skipped = %v", skipped)
	}
}
