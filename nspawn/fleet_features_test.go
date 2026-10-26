package nspawn

import (
	"sort"
	"strings"
	"testing"
)

func TestFleetFeaturesSorted(t *testing.T) {
	if len(fleetFeatures) == 0 || !sort.StringsAreSorted(fleetFeatures) {
		t.Fatalf("features must be non-empty and sorted: %v", fleetFeatures)
	}
	for _, f := range fleetFeatures {
		if f == "" || strings.ContainsAny(f, " ,") {
			t.Fatalf("bad feature name %q", f)
		}
	}
	want := "address-families,cpu-hard-limit,cpu-weight,env-names,exec-env,hostname,load-credentials,portmap-bridge,scope-stats,store-lower,store-root,tasks-max,unprivileged-ports,userns-netns"
	if got := fleetFeaturesAttr(); got != want {
		t.Fatalf("got %q", got)
	}
}
