package nspawn

import (
	"fmt"
	"regexp"
	"strings"
)

// fleetAddrFamilyRe is one entry of restrict_address_families: an address
// family name, with an optional "~" prefix that denies it.
var fleetAddrFamilyRe = regexp.MustCompile(`^~?AF_[A-Z0-9_]+$`)

// fleetValidateAddrFamilies checks the restrict_address_families task option.
// An empty list is allowed and passes no argument (the nspawn default). The
// special value "none" denies every family and must be the only entry.
func fleetValidateAddrFamilies(l []string) error {
	if len(l) == 1 && l[0] == "none" {
		return nil
	}
	for _, f := range l {
		if !fleetAddrFamilyRe.MatchString(f) {
			return fmt.Errorf("restrict_address_families: %q is not an address family (AF_<NAME>, ~AF_<NAME>, or \"none\" alone)", f)
		}
	}
	return nil
}

// fleetAddrFamilyArgs returns the nspawn argument for the
// restrict_address_families option. systemd-nspawn takes a space separated
// list. The option needs systemd 261 or later.
func fleetAddrFamilyArgs(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	return []string{"--restrict-address-families=" + strings.Join(l, " ")}
}
