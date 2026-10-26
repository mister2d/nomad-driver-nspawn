package nspawn

import (
	"strings"
	"testing"
)

func TestFleetAddrFamiliesValidate(t *testing.T) {
	good := [][]string{
		nil,
		{"none"},
		{"AF_INET", "AF_INET6", "AF_UNIX", "AF_NETLINK"},
		{"~AF_PACKET"},
	}
	for _, l := range good {
		if err := fleetValidateAddrFamilies(l); err != nil {
			t.Errorf("%q: %v", l, err)
		}
	}
	bad := [][]string{
		{"AF_INET", "none"},
		{"af_inet"},
		{"INET"},
		{"AF_INET AF_UNIX"},
		{""},
		{"~"},
	}
	for _, l := range bad {
		if err := fleetValidateAddrFamilies(l); err == nil {
			t.Errorf("%q: want an error", l)
		}
	}
}

func TestFleetAddrFamilyArgs(t *testing.T) {
	if got := fleetAddrFamilyArgs(nil); got != nil {
		t.Errorf("empty: got %v", got)
	}
	got := fleetAddrFamilyArgs([]string{"AF_INET", "AF_UNIX"})
	if len(got) != 1 || got[0] != "--restrict-address-families=AF_INET AF_UNIX" {
		t.Errorf("got %v", got)
	}
}

func TestFleetAddrFamiliesFeature(t *testing.T) {
	if !strings.Contains(","+fleetFeaturesAttr()+",", ",address-families,") {
		t.Error("features attribute must list address-families")
	}
}
