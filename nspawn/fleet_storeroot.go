package nspawn

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Store root mode (root_mode = "store").
//
// The machine root is a small tmpfs per task. The image only supplies its
// /nix/store, which nspawn binds read-only with an ID mapping. PID 1 is the
// init of a NixOS toplevel in that store. This is the shape of NixOS
// declarative containers. It lets --private-users=pick work on a shared image:
// overlayfs is not used, so nspawn never has to idmap an overlay mount (Linux
// overlayfs has no FS_ALLOW_IDMAP), and nothing is chowned on the image.

const (
	fleetRootModeImage = "image"
	fleetRootModeStore = "store"

	fleetStoreRootDirName     = "nspawn-root"
	fleetDefaultStoreRootSize = "256m"
	fleetSystemProfile        = "/nix/var/nix/profiles/system"
	fleetStorePrefix          = "/nix/store/"
	fleetMaxLinkHops          = 16
)

var fleetSizeRe = regexp.MustCompile(`^[1-9][0-9]*[kKmMgG]?$`)

// fleetStoreRootSkeleton lists the directories that NixOS stage 2 and
// activation expect on the root. Everything else is created at boot.
var fleetStoreRootSkeleton = []struct {
	path string
	mode os.FileMode
}{
	{"bin", 0o755},
	{"etc", 0o755},
	{"nix/var/nix/gcroots", 0o755},
	{"nix/var/nix/profiles", 0o755},
	{"root", 0o700},
	{"tmp", 0o1777},
	{"usr/bin", 0o755},
	{"var/lib", 0o755},
}

// fleetStoreRootDir returns the tmpfs mount point for a task.
func fleetStoreRootDir(taskDir string) string {
	return filepath.Join(taskDir, fleetStoreRootDirName)
}

// fleetValidateStoreRoot checks the options that store mode accepts.
func fleetValidateStoreRoot(c *MachineConfig) error {
	switch c.RootMode {
	case "", fleetRootModeImage:
		if c.Toplevel != "" || c.RootSize != "" {
			return fmt.Errorf("toplevel and root_size need root_mode = %q", fleetRootModeStore)
		}
		return nil
	case fleetRootModeStore:
	default:
		return fmt.Errorf("invalid parameter for root_mode")
	}
	if c.Ephemeral || c.Volatile != "" || c.ReadOnly || c.ProcessTwo || len(c.Command) > 0 || c.ImageDownload != nil {
		return fmt.Errorf("root_mode = %q may not be combined with ephemeral, volatile, read_only, process_two, command or image_download", fleetRootModeStore)
	}
	if c.PrivateUsersOwnership != "" && c.PrivateUsersOwnership != "map" {
		return fmt.Errorf("root_mode = %q needs private_users_ownership = \"map\"", fleetRootModeStore)
	}
	if c.Toplevel != "" && !fleetIsStorePath(c.Toplevel) {
		return fmt.Errorf("toplevel must be a /nix/store/<name> path")
	}
	if c.RootSize != "" && !fleetSizeRe.MatchString(c.RootSize) {
		return fmt.Errorf("invalid parameter for root_size")
	}
	return nil
}

// fleetIsStorePath reports whether p is exactly /nix/store/<name>.
func fleetIsStorePath(p string) bool {
	if !strings.HasPrefix(p, fleetStorePrefix) {
		return false
	}
	name := strings.TrimPrefix(p, fleetStorePrefix)
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

// fleetResolveToplevel returns the toplevel store path. An explicit value
// wins. Otherwise the system profile link chain in the image is followed.
// Every link is resolved inside the image, never on the host. The toplevel
// must contain an init.
func fleetResolveToplevel(image, toplevel string) (string, error) {
	p := toplevel
	if p == "" {
		cur := fleetSystemProfile
		for hop := 0; ; hop++ {
			if hop == fleetMaxLinkHops {
				return "", fmt.Errorf("too many links below %s in the image", fleetSystemProfile)
			}
			target, err := os.Readlink(filepath.Join(image, cur))
			if err != nil {
				return "", fmt.Errorf("cannot read %s in the image: %v", cur, err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			cur = filepath.Clean(target)
			if fleetIsStorePath(cur) {
				break
			}
		}
		p = cur
	}
	if !fleetIsStorePath(p) {
		return "", fmt.Errorf("toplevel %q is not a /nix/store/<name> path", p)
	}
	st, err := os.Lstat(filepath.Join(image, p, "init"))
	if err != nil {
		return "", fmt.Errorf("toplevel %s has no init in the image: %v", p, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("toplevel %s init is a directory", p)
	}
	return p, nil
}

// fleetStoreRootArgs returns the nspawn arguments for store mode. The root
// directory comes first, the init last.
func fleetStoreRootArgs(rootDir, image, toplevel string) (head, tail []string) {
	head = []string{"-D", rootDir}
	tail = []string{
		"--bind-ro=" + filepath.Join(image, "nix/store") + ":/nix/store:idmap",
		// Without --boot nspawn sends SIGKILL on stop. systemd as PID 1 needs
		// SIGRTMIN+3 for a clean shutdown.
		"--kill-signal=SIGRTMIN+3",
		toplevel + "/init",
	}
	return head, tail
}

// fleetPrepareStoreRoot mounts the tmpfs root and creates the skeleton. A
// mount that already exists (task restart) is reused after a remount.
func fleetPrepareStoreRoot(dir, size string) error {
	if size == "" {
		size = fleetDefaultStoreRootSize
	}
	if !fleetSizeRe.MatchString(size) {
		return fmt.Errorf("invalid root_size %q", size)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fleetCleanupStoreRoot(dir)
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size="+size); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %v", dir, err)
	}
	for _, s := range fleetStoreRootSkeleton {
		p := filepath.Join(dir, s.path)
		if err := os.MkdirAll(p, 0o755); err != nil {
			fleetCleanupStoreRoot(dir)
			return err
		}
		if err := os.Chmod(p, s.mode); err != nil {
			fleetCleanupStoreRoot(dir)
			return err
		}
	}
	for _, f := range []string{"etc/os-release", "etc/machine-id"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o444); err != nil {
			fleetCleanupStoreRoot(dir)
			return err
		}
	}
	return nil
}

// fleetCleanupStoreRoot detaches the tmpfs root. It ignores a missing mount.
func fleetCleanupStoreRoot(dir string) {
	for {
		if err := unix.Unmount(dir, unix.MNT_DETACH); err != nil {
			return
		}
	}
}

// fleetSetUnprivilegedPortStart writes net.ipv4.ip_unprivileged_port_start in
// the network namespace at nsPath. Inside a user namespace the machine root
// has no CAP_NET_BIND_SERVICE in a namespace that Nomad owns, so sshd cannot
// bind port 22 without this.
func fleetSetUnprivilegedPortStart(nsPath string, port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("unprivileged_port_start must be 0..65535")
	}
	errc := make(chan error, 1)
	go func() {
		// The thread stays locked and ends with the goroutine if the switch
		// back fails, so no other goroutine runs in the task namespace.
		runtime.LockOSThread()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			errc <- err
			return
		}
		defer orig.Close()
		target, err := os.Open(nsPath)
		if err != nil {
			errc <- err
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- fmt.Errorf("setns %s: %v", nsPath, err)
			return
		}
		// /proc/sys/net shows the network namespace of the calling thread.
		werr := os.WriteFile("/proc/sys/net/ipv4/ip_unprivileged_port_start",
			[]byte(strconv.Itoa(port)), 0o644)
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- fmt.Errorf("setns back: %v", err)
			return
		}
		runtime.UnlockOSThread()
		errc <- werr
	}()
	return <-errc
}
