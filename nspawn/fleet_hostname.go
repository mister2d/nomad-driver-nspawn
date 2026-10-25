package nspawn

import (
	"fmt"
	"regexp"
	"strings"
)

// fleetHostnameMax is the kernel limit for a UTS hostname (HOST_NAME_MAX).
const fleetHostnameMax = 64

// fleetHostnameLabelRe is one RFC 1123 label: letters, digits and hyphens,
// 1 to 63 characters, no hyphen at the start or the end.
var fleetHostnameLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// fleetValidateHostname checks the hostname task option. Nomad interpolates
// the task config before the driver gets it, so "${" means a variable that
// Nomad did not know. An empty value is allowed and keeps the nspawn default
// (the machine name).
func fleetValidateHostname(h string) error {
	if h == "" {
		return nil
	}
	if strings.Contains(h, "${") {
		return fmt.Errorf("hostname %q has an unresolved variable", h)
	}
	if len(h) > fleetHostnameMax {
		return fmt.Errorf("hostname %q is longer than %d characters", h, fleetHostnameMax)
	}
	for _, l := range strings.Split(h, ".") {
		if !fleetHostnameLabelRe.MatchString(l) {
			return fmt.Errorf("hostname %q is not a valid DNS name", h)
		}
	}
	return nil
}

// fleetHostnameArgs returns the nspawn argument for the hostname option.
func fleetHostnameArgs(h string) []string {
	if h == "" {
		return nil
	}
	return []string{"--hostname=" + h}
}
