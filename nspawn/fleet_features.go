package nspawn

import "strings"

// fleetFeatures lists the capabilities added on the fleet branch. The list is
// sorted. Jobs can require one with the constraint
// attr.driver.nspawn.features set_contains <name>.
var fleetFeatures = []string{
	"cpu-hard-limit",
	"cpu-weight",
	"env-names",
	"exec-env",
	"hostname",
	"load-credentials",
	"portmap-bridge",
	"scope-stats",
	"store-lower",
	"store-root",
	"tasks-max",
	"unprivileged-ports",
	"userns-netns",
}

func fleetFeaturesAttr() string {
	return strings.Join(fleetFeatures, ",")
}
