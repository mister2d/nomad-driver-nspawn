package nspawn

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// fleetCredentialIDRe matches the characters systemd allows in a credential
// name. A leading dot is rejected separately.
var fleetCredentialIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// fleetValidateCredentialID checks a credential ID against the systemd rules.
func fleetValidateCredentialID(id string) error {
	if id == "" {
		return fmt.Errorf("credential ID must not be empty")
	}
	if len(id) > 255 {
		return fmt.Errorf("credential ID %q is longer than 255 bytes", id)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("credential ID %q must not start with a dot", id)
	}
	if !fleetCredentialIDRe.MatchString(id) {
		return fmt.Errorf("credential ID %q may contain only letters, digits, '_', '.' and '-'", id)
	}
	return nil
}

// fleetUnder reports whether path lies inside root (both must be clean and
// absolute).
func fleetUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// fleetResolveCredentialPath resolves p to an absolute path of a regular file
// inside one of the allowed roots (the task local and secrets directories).
// A relative p is resolved against taskDir. Symbolic links are followed and
// the real path must still be inside a root.
func fleetResolveCredentialPath(taskDir string, roots []string, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if strings.ContainsRune(p, ':') {
		return "", fmt.Errorf("path %q must not contain ':'", p)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(taskDir, p)
	}
	p = filepath.Clean(p)

	realRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		realRoots = append(realRoots, filepath.Clean(rr))
	}
	if len(realRoots) == 0 {
		return "", fmt.Errorf("no readable task directory")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %v", p, err)
	}
	real = filepath.Clean(real)
	inside := false
	for _, r := range realRoots {
		if fleetUnder(r, real) {
			inside = true
			break
		}
	}
	if !inside {
		return "", fmt.Errorf("path %q resolves to %q outside the task local and secrets directories", p, real)
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("path %q is not a regular file", p)
	}
	return real, nil
}

// fleetLoadCredentialArgs validates all load_credentials entries and returns
// the --load-credential arguments in sorted ID order.
func fleetLoadCredentialArgs(taskDir string, roots []string, creds map[string]string) ([]string, error) {
	ids := make([]string, 0, len(creds))
	for id := range creds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	args := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := fleetValidateCredentialID(id); err != nil {
			return nil, err
		}
		p, err := fleetResolveCredentialPath(taskDir, roots, creds[id])
		if err != nil {
			return nil, fmt.Errorf("load_credentials %q: %v", id, err)
		}
		args = append(args, "--load-credential="+id+":"+p)
	}
	return args, nil
}

// fleetSecretIDRe flags IDs that suggest secret content. set_credentials
// values appear in the host process table, so such IDs must use a file.
var fleetSecretIDRe = regexp.MustCompile(`(?i)(pass|secret|token|key|private|cert)`)

// fleetSetCredentialArgs validates set_credentials entries and returns the
// --set-credential arguments in sorted ID order. IDs that overlap with
// load_credentials are refused.
func fleetSetCredentialArgs(creds, loaded map[string]string) ([]string, error) {
	ids := make([]string, 0, len(creds))
	for id := range creds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	args := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := fleetValidateCredentialID(id); err != nil {
			return nil, err
		}
		if _, dup := loaded[id]; dup {
			return nil, fmt.Errorf("credential %q is in both load_credentials and set_credentials", id)
		}
		if fleetSecretIDRe.MatchString(id) {
			return nil, fmt.Errorf("set_credentials %q looks like secret data, use load_credentials with a file in secrets/", id)
		}
		args = append(args, "--set-credential="+id+":"+creds[id])
	}
	return args, nil
}

// fleetRedactArgs returns a copy of args with --set-credential values hidden,
// for logging.
func fleetRedactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "--set-credential=") {
			if j := strings.Index(a, ":"); j >= 0 {
				a = a[:j+1] + "<redacted>"
			}
		}
		out[i] = a
	}
	return out
}
