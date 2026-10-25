package nspawn

import (
	"strings"
	"testing"
)

func TestFleetHostnameValidate(t *testing.T) {
	good := []string{"", "sbx-07b04753", "a", "sbx-1.home.lan", strings.Repeat("a", 63)}
	for _, h := range good {
		if err := fleetValidateHostname(h); err != nil {
			t.Errorf("%q: %v", h, err)
		}
	}
	bad := []string{
		"sbx-${NOMAD_UNKNOWN}",
		"-sbx", "sbx-", "sbx_1", "sbx 1", "sbx..lan", ".sbx",
		strings.Repeat("a", 64),
		strings.Repeat("a.", 32) + "a",
	}
	for _, h := range bad {
		if err := fleetValidateHostname(h); err == nil {
			t.Errorf("%q: want an error", h)
		}
	}
}

func TestFleetHostnameArgs(t *testing.T) {
	if got := fleetHostnameArgs(""); got != nil {
		t.Errorf("empty: got %v", got)
	}
	got := fleetHostnameArgs("sbx-07b04753")
	if len(got) != 1 || got[0] != "--hostname=sbx-07b04753" {
		t.Errorf("got %v", got)
	}
}

func TestFleetHostnameFeature(t *testing.T) {
	if !strings.Contains(","+fleetFeaturesAttr()+",", ",hostname,") {
		t.Error("features attribute must list hostname")
	}
}
