package nspawn

// fleetDefaultExecPath lists NixOS locations first. The classic locations
// follow, so Debian and Alpine images resolve commands too. Missing
// directories in PATH are harmless.
const fleetDefaultExecPath = "/run/current-system/sw/bin:/run/wrappers/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// fleetExecEnv returns the NAME=VALUE pairs for systemd-run --setenv. A
// transient service that systemd-run starts in a machine gets no PATH on
// NixOS. A value that the task environment sets wins over the default.
func fleetExecEnv(taskEnv map[string]string, tty bool) []string {
	defaults := []struct{ k, v string }{
		{"PATH", fleetDefaultExecPath},
		{"HOME", "/root"},
		{"USER", "root"},
		{"LOGNAME", "root"},
	}
	var out []string
	for _, d := range defaults {
		v := d.v
		if tv, ok := taskEnv[d.k]; ok && tv != "" {
			v = tv
		}
		out = append(out, d.k+"="+v)
	}
	if tty {
		v := "xterm-256color"
		if tv, ok := taskEnv["TERM"]; ok && tv != "" {
			v = tv
		}
		out = append(out, "TERM="+v)
	}
	return out
}

// fleetExecCommand builds the systemd-run command line for exec in a
// machine. The order is: fixed flags, machine, pipe or pty flags,
// --setenv entries, then the user command.
func fleetExecCommand(machine string, command []string, tty bool, taskEnv map[string]string) []string {
	cmd := []string{"systemd-run", "--wait", "--service-type=exec",
		"--collect", "--quiet", "--machine", machine}
	if tty {
		cmd = append(cmd, "--pty", "--send-sighup")
	} else {
		cmd = append(cmd, "--pipe")
	}
	for _, e := range fleetExecEnv(taskEnv, tty) {
		cmd = append(cmd, "--setenv="+e)
	}
	return append(cmd, command...)
}

// fleetTaskEnv returns the environment of the task config. It returns nil
// when the config cannot be decoded.
func fleetTaskEnv(handle *taskHandle) map[string]string {
	var dc MachineConfig
	if err := handle.taskConfig.DecodeDriverConfig(&dc); err != nil {
		return nil
	}
	return dc.Environment
}
