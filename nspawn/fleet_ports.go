package nspawn

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// fleetPortEnvName returns the name of the environment variable that Nomad
// sets for the host port of a port label. Nomad replaces every character
// that is not a letter, digit or underscore with an underscore.
func fleetPortEnvName(label string) string {
	b := []byte(label)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			b[i] = '_'
		}
	}
	return "NOMAD_HOST_PORT_" + string(b)
}

// fleetPortMapFromEnv builds the label to "host:machine" mapping for a
// port_map when the task has no network resources (bridge mode). The host
// port comes from NOMAD_HOST_PORT_<label> in env. The machine port comes
// from portMap. One error names every label without a usable host port.
func fleetPortMapFromEnv(portMap map[string]int, env map[string]string) (map[string]string, error) {
	labels := make([]string, 0, len(portMap))
	for l := range portMap {
		labels = append(labels, l)
	}
	sort.Strings(labels)

	out := make(map[string]string, len(labels))
	var missing []string
	for _, l := range labels {
		raw := env[fleetPortEnvName(l)]
		host, err := strconv.Atoi(raw)
		if err != nil || host <= 0 || host > 65535 {
			missing = append(missing, l)
			continue
		}
		out[l] = fmt.Sprintf("%d:%d", host, portMap[l])
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("port_map labels without an allocated port: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
