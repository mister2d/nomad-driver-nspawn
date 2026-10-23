package nspawn

import "strings"

// fleetEnvName returns a name that systemd-nspawn --setenv accepts. systemd allows only
// [A-Za-z0-9_] and no leading digit (env_name_is_valid, VALID_BASH_ENV_NAME_CHARS). Nomad builds
// names from task names, for example NOMAD_ALLOC_IP_connect-proxy-<service> next to a Connect
// sidecar, and nspawn then refuses to start ("Cannot assign environment variable"). Invalid
// characters become '_'. The second result is false when the name is already valid.
func fleetEnvName(name string) (string, bool) {
	var b strings.Builder
	changed := false
	for i, r := range name {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 && r >= '0' && r <= '9' {
			b.WriteByte('_')
			changed = true
		}
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
			changed = true
		}
	}
	return b.String(), changed
}

// fleetMergeTaskEnv copies the Nomad task environment into dst under names systemd accepts. A renamed
// variable is skipped when its new name is already set by Nomad or by the job, so a real value
// is never overwritten. It returns the original names that were skipped.
func fleetMergeTaskEnv(dst map[string]string, env map[string]string) []string {
	var skipped []string
	for k, v := range env {
		n, changed := fleetEnvName(k)
		if n == "" {
			skipped = append(skipped, k)
			continue
		}
		if changed {
			if _, taken := env[n]; taken {
				skipped = append(skipped, k)
				continue
			}
			if _, taken := dst[n]; taken {
				skipped = append(skipped, k)
				continue
			}
		}
		dst[n] = v
	}
	return skipped
}
