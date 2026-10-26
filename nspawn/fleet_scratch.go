package nspawn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
	"golang.org/x/sys/unix"
)

// Scratch volumes (task option scratch, plugin block scratch).
//
// Each scratch volume is a thin RBD image that lives exactly as long as its
// task. The driver creates, maps (krbd), formats (ext4) and mounts it on the
// host before nspawn starts, and binds directories of it into the machine with
// an ID mapping. After the machine is gone the driver unmounts, unmaps and
// removes the image. It does not use the RBD trash: trashed images keep their
// data but drop out of the budget count, and krbd writes hang (not fail) when
// the pool quota is full (spike on prime, 2026-10-26). `rbd trash mv
// --expires-at` also runs /bin/date, which NixOS does not have.
//
// Safety:
//   - The cephx key of the node reaches one RBD namespace only. Image names
//     are <alloc id>-<task hash>-<volume>. The driver touches no other name.
//   - A per-node budget caps the provisioned size. The driver never
//     overcommits it.
//   - Release works by name prefix and needs no saved state. The sweeper
//     releases a prefix only when no task of this driver owns it, the machine
//     is gone and the plugin has run longer than the grace period (so that
//     RecoverTask has run). An image with a watcher is never removed. Ceph
//     also refuses to remove an image with a watcher.

const (
	fleetScratchDefaultMountDir      = "/run/nspawn-scratch"
	fleetScratchDefaultSweepInterval = 10 * time.Minute
	fleetScratchDefaultSweepGrace    = 15 * time.Minute
	fleetScratchDefaultOpTimeout     = 60 * time.Second
	fleetScratchMkfsTimeout          = 5 * time.Minute
	fleetScratchHealthTTL            = time.Minute
	// fleetScratchMaxVolumes and fleetScratchMaxSize are the hard limits of
	// the driver. The plugin options max_volumes and max_volume_size set a
	// node ceiling at or below them (default: the hard limit).
	fleetScratchMaxVolumes = 4
	fleetScratchMinSize    = int64(1) << 30
	fleetScratchMaxSize    = int64(1) << 40
	fleetMachinesStateDir  = "/run/systemd/machines"
)

var (
	fleetScratchVolNameRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	fleetScratchImageRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-[0-9a-f]{8}-[a-z][a-z0-9]{0,15}$`)
	fleetBytesRe          = regexp.MustCompile(`^([1-9][0-9]*)([MGT])$`)
	fleetCephNameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// Targets that the driver or Nomad binds. A scratch bind must not shadow them.
	fleetScratchReservedTargets = []string{"/nix/store", "/alloc", "/local", "/secrets"}
)

// FleetScratchConfig is the plugin block scratch.
type FleetScratchConfig struct {
	Pool          string `codec:"pool"`
	Namespace     string `codec:"namespace"`
	ClientID      string `codec:"client_id"`
	Keyring       string `codec:"keyring"`
	Budget        string `codec:"budget"`
	MountDir      string `codec:"mount_dir"`
	SweepInterval string `codec:"sweep_interval"`
	SweepGrace    string `codec:"sweep_grace"`
	OpTimeout     string `codec:"op_timeout"`
	MaxVolumes    int64  `codec:"max_volumes"`
	MaxVolumeSize string `codec:"max_volume_size"`
}

// FleetScratchVolume is one task block scratch.
type FleetScratchVolume struct {
	Name string             `codec:"name"`
	Size string             `codec:"size"`
	Bind hclutils.MapStrStr `codec:"bind"`
}

// fleetParseBytes parses <n>M, <n>G or <n>T (binary units).
func fleetParseBytes(s string) (int64, error) {
	m := fleetBytesRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q: use <n>M, <n>G or <n>T", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %v", s, err)
	}
	shift := map[string]uint{"M": 20, "G": 30, "T": 40}[m[2]]
	if n > (int64(1)<<62)>>shift {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n << shift, nil
}

// fleetScratchTaskHash identifies the task inside the allocation.
func fleetScratchTaskHash(task string) string {
	h := fnv.New32a()
	h.Write([]byte(task))
	return fmt.Sprintf("%08x", h.Sum32())
}

// fleetScratchPrefix is the name prefix of every image of one task.
func fleetScratchPrefix(allocID, task string) string {
	return allocID + "-" + fleetScratchTaskHash(task) + "-"
}

// fleetScratchPrefixOf returns the task prefix of an image name, or "".
func fleetScratchPrefixOf(image string) string {
	if !fleetScratchImageRe.MatchString(image) {
		return ""
	}
	// <36 alloc>-<8 hash>-<vol>
	return image[:36+1+8+1]
}

// fleetValidateScratch checks the task scratch blocks.
func fleetValidateScratch(c *MachineConfig) error {
	if len(c.Scratch) == 0 {
		return nil
	}
	if c.RootMode != fleetRootModeStore {
		return fmt.Errorf("scratch needs root_mode = %q", fleetRootModeStore)
	}
	if !c.UserNamespacing {
		return fmt.Errorf("scratch needs user_namespacing")
	}
	if len(c.Scratch) > fleetScratchMaxVolumes {
		return fmt.Errorf("at most %d scratch volumes", fleetScratchMaxVolumes)
	}
	names := map[string]bool{}
	targets := map[string]bool{}
	for _, v := range c.Scratch {
		if !fleetScratchVolNameRe.MatchString(v.Name) {
			return fmt.Errorf("scratch name %q: use 1 to 16 of a-z and 0-9, starting with a letter", v.Name)
		}
		if names[v.Name] {
			return fmt.Errorf("scratch name %q is used twice", v.Name)
		}
		names[v.Name] = true
		size, err := fleetParseBytes(v.Size)
		if err != nil {
			return fmt.Errorf("scratch %q: %v", v.Name, err)
		}
		if size < fleetScratchMinSize || size > fleetScratchMaxSize {
			return fmt.Errorf("scratch %q: size must be 1G to 1T", v.Name)
		}
		if len(v.Bind) == 0 {
			return fmt.Errorf("scratch %q: bind is empty", v.Name)
		}
		for sub, target := range v.Bind {
			if strings.ContainsAny(sub, ":,") || strings.ContainsAny(target, ":,") {
				return fmt.Errorf("scratch %q: bind paths may not contain ':' or ','", v.Name)
			}
			for _, r := range fleetScratchReservedTargets {
				if target == r || strings.HasPrefix(target, r+"/") {
					return fmt.Errorf("scratch %q: bind target %q is reserved", v.Name, target)
				}
			}
			if sub != "." && (filepath.IsAbs(sub) || filepath.Clean(sub) != sub || sub == ".." || strings.HasPrefix(sub, "../")) {
				return fmt.Errorf("scratch %q: bind source %q must be \".\" or a clean relative path", v.Name, sub)
			}
			if !filepath.IsAbs(target) || filepath.Clean(target) != target || target == "/" {
				return fmt.Errorf("scratch %q: bind target %q must be a clean absolute path other than /", v.Name, target)
			}
			if targets[target] {
				return fmt.Errorf("scratch bind target %q is used twice", target)
			}
			targets[target] = true
		}
	}
	return nil
}

type fleetScratchBind struct {
	source, target string
}

// fleetScratchBinds returns the host source and machine target of every
// scratch bind, parents before children.
func fleetScratchBinds(mountDir, prefix string, vols []FleetScratchVolume) []fleetScratchBind {
	var out []fleetScratchBind
	for _, v := range vols {
		base := filepath.Join(mountDir, prefix+v.Name)
		for sub, target := range v.Bind {
			out = append(out, fleetScratchBind{source: filepath.Join(base, sub), target: target})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].target) != len(out[j].target) {
			return len(out[i].target) < len(out[j].target)
		}
		return out[i].target < out[j].target
	})
	return out
}

// fleetScratchBindArgs returns the nspawn arguments for the scratch binds.
func fleetScratchBindArgs(binds []fleetScratchBind) []string {
	args := make([]string, 0, len(binds))
	for _, b := range binds {
		args = append(args, "--bind="+b.source+":"+b.target+":idmap")
	}
	return args
}

// fleetScratchDirMode returns the mode of a bind source directory. A
// temporary directory target gets the sticky, world-writable mode.
func fleetScratchDirMode(target string) os.FileMode {
	if target == "/tmp" || target == "/var/tmp" {
		return os.ModeSticky | 0o777
	}
	return 0o755
}

// fleetRunner runs an external command and returns its standard output.
type fleetRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type fleetExecRunner struct{}

func (fleetExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// fleetScratch is the parsed plugin block plus the runtime state.
type fleetScratch struct {
	pool, namespace, clientID, keyring, mountDir string
	budget                                       int64
	maxVolumes                                   int
	maxVolumeSize                                int64
	sweepInterval, sweepGrace                    time.Duration
	opTimeout                                    time.Duration

	runner      fleetRunner
	logger      hclog.Logger
	startedAt   time.Time
	sysfsDir    string
	machinesDir string
	mount       func(dev, dir string) error
	unmount     func(dir string) error
	now         func() time.Time

	// mu serializes the budget check and the image creation.
	mu sync.Mutex

	// inflight holds the prefixes of starts that are not yet in the task
	// store. The sweeper never touches them. Tags cannot be changed after the
	// map: the kernel holds the exclusive lock, and image-meta set from the
	// CLI gets EROFS.
	inflightMu sync.Mutex
	inflight   map[string]bool

	healthMu   sync.Mutex
	healthAt   time.Time
	healthErr  error
	sweepStart sync.Once
}

func fleetDurationOr(s string, def time.Duration, allowZero bool) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 || (d == 0 && !allowZero) {
		return 0, fmt.Errorf("duration %q is out of range", s)
	}
	return d, nil
}

// newFleetScratch checks the plugin block. A nil block gives nil.
func newFleetScratch(c *FleetScratchConfig, logger hclog.Logger) (*fleetScratch, error) {
	if c == nil {
		return nil, nil
	}
	for k, v := range map[string]string{"pool": c.Pool, "namespace": c.Namespace, "client_id": c.ClientID} {
		if !fleetCephNameRe.MatchString(v) {
			return nil, fmt.Errorf("scratch %s %q is not a valid Ceph name", k, v)
		}
	}
	if !filepath.IsAbs(c.Keyring) {
		return nil, fmt.Errorf("scratch keyring must be an absolute path")
	}
	budget, err := fleetParseBytes(c.Budget)
	if err != nil {
		return nil, fmt.Errorf("scratch budget: %v", err)
	}
	s := &fleetScratch{
		pool: c.Pool, namespace: c.Namespace, clientID: c.ClientID, keyring: c.Keyring,
		mountDir: c.MountDir, budget: budget,
		runner: fleetExecRunner{}, logger: logger, startedAt: time.Now(),
		sysfsDir: "/sys/bus/rbd/devices", machinesDir: fleetMachinesStateDir,
		mount: func(dev, dir string) error {
			return unix.Mount(dev, dir, "ext4", unix.MS_NODEV|unix.MS_NOSUID|unix.MS_NOATIME, "")
		},
		unmount: func(dir string) error { return unix.Unmount(dir, 0) },
		now:     time.Now,
	}
	if s.mountDir == "" {
		s.mountDir = fleetScratchDefaultMountDir
	}
	if !filepath.IsAbs(s.mountDir) || filepath.Clean(s.mountDir) != s.mountDir || s.mountDir == "/" {
		return nil, fmt.Errorf("scratch mount_dir must be a clean absolute path")
	}
	if s.sweepInterval, err = fleetDurationOr(c.SweepInterval, fleetScratchDefaultSweepInterval, false); err != nil {
		return nil, fmt.Errorf("scratch sweep_interval: %v", err)
	}
	if s.sweepGrace, err = fleetDurationOr(c.SweepGrace, fleetScratchDefaultSweepGrace, false); err != nil {
		return nil, fmt.Errorf("scratch sweep_grace: %v", err)
	}
	if s.opTimeout, err = fleetDurationOr(c.OpTimeout, fleetScratchDefaultOpTimeout, false); err != nil {
		return nil, fmt.Errorf("scratch op_timeout: %v", err)
	}
	s.maxVolumes = fleetScratchMaxVolumes
	if c.MaxVolumes != 0 {
		if c.MaxVolumes < 1 || c.MaxVolumes > fleetScratchMaxVolumes {
			return nil, fmt.Errorf("scratch max_volumes must be 1 to %d", fleetScratchMaxVolumes)
		}
		s.maxVolumes = int(c.MaxVolumes)
	}
	s.maxVolumeSize = fleetScratchMaxSize
	if c.MaxVolumeSize != "" {
		n, err := fleetParseBytes(c.MaxVolumeSize)
		if err != nil {
			return nil, fmt.Errorf("scratch max_volume_size: %v", err)
		}
		if n < fleetScratchMinSize || n > fleetScratchMaxSize {
			return nil, fmt.Errorf("scratch max_volume_size must be 1G to 1T")
		}
		s.maxVolumeSize = n
	}
	return s, nil
}

func (s *fleetScratch) auth() []string {
	return []string{"--id", s.clientID, "--keyring", s.keyring}
}

func (s *fleetScratch) scoped(args ...string) []string {
	return append(append(s.auth(), "--pool", s.pool, "--namespace", s.namespace), args...)
}

func (s *fleetScratch) rbd(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	if timeout == 0 {
		timeout = s.opTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.runner.Run(ctx, "rbd", args...)
}

type fleetRBDImage struct {
	Image string `json:"image"`
	Size  int64  `json:"size"`
}

// images lists the images in the namespace with their sizes. rbd ls --long
// opens each image, so it fails with ENOENT when another task removes an
// image during the listing (seen on prime with three stops at once). The
// call is tried three times.
func (s *fleetScratch) images(ctx context.Context) ([]fleetRBDImage, error) {
	var out []byte
	var err error
	for i := 0; i < 3; i++ {
		if out, err = s.rbd(ctx, 0, s.scoped("ls", "--long", "--format", "json")...); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return nil, err
	}
	var imgs []fleetRBDImage
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(out, &imgs); err != nil {
		return nil, fmt.Errorf("rbd ls: %v", err)
	}
	return imgs, nil
}

// names lists the image names in the namespace. It opens no image, so a
// concurrent removal cannot make it fail.
func (s *fleetScratch) names(ctx context.Context) ([]string, error) {
	out, err := s.rbd(ctx, 0, s.scoped("ls", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	var names []string
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(out, &names); err != nil {
		return nil, fmt.Errorf("rbd ls: %v", err)
	}
	return names, nil
}

// Health returns the last health result. It never blocks on Ceph. Before the
// first check it reports an error.
func (s *fleetScratch) Health() error {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.healthAt.IsZero() {
		return errors.New("scratch: first health check not done")
	}
	return s.healthErr
}

// StartHealth checks the key at once and then every fleetScratchHealthTTL.
func (s *fleetScratch) StartHealth(ctx context.Context) {
	go func() {
		for {
			s.checkHealth(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(fleetScratchHealthTTL):
			}
		}
	}()
}

func (s *fleetScratch) checkHealth(ctx context.Context) {
	err := func() error {
		st, err := os.Stat(s.keyring)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("keyring %s is not a regular file", s.keyring)
		}
		_, err = s.rbd(ctx, 15*time.Second, s.scoped("ls")...)
		return err
	}()
	s.healthMu.Lock()
	s.healthAt, s.healthErr = s.now(), err
	s.healthMu.Unlock()
}

// Provision creates, maps, formats and mounts the volumes of one task. On
// error it releases what it made.
//
// The prefix stays in flight until the caller calls Settle, after the task is
// in the task store or the start has failed.
func (s *fleetScratch) Provision(ctx context.Context, allocID, task, machine string, vols []FleetScratchVolume, emit func(string)) error {
	if err := s.checkCeilings(vols); err != nil {
		return err
	}
	prefix := fleetScratchPrefix(allocID, task)
	s.inflightMu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]bool{}
	}
	s.inflight[prefix] = true
	s.inflightMu.Unlock()
	if err := s.provision(ctx, prefix, machine, vols, emit); err != nil {
		if rerr := s.Release(ctx, prefix); rerr != nil {
			s.logger.Error("scratch: cleanup after a failed start", "prefix", prefix, "error", rerr)
		}
		s.Settle(prefix)
		return err
	}
	return nil
}

// checkCeilings applies the node ceilings max_volumes and max_volume_size.
// Validate already applied the hard limits of the driver.
func (s *fleetScratch) checkCeilings(vols []FleetScratchVolume) error {
	if len(vols) > s.maxVolumes {
		return fmt.Errorf("scratch: %d volumes, the node allows %d (max_volumes)", len(vols), s.maxVolumes)
	}
	for _, v := range vols {
		n, err := fleetParseBytes(v.Size)
		if err != nil {
			return fmt.Errorf("scratch %q: %v", v.Name, err)
		}
		if n > s.maxVolumeSize {
			return fmt.Errorf("scratch %q: size %s is above the node ceiling of %d bytes (max_volume_size)", v.Name, v.Size, s.maxVolumeSize)
		}
	}
	return nil
}

func (s *fleetScratch) provision(ctx context.Context, prefix, machine string, vols []FleetScratchVolume, emit func(string)) error {
	var need int64
	sizes := make([]int64, len(vols))
	for i, v := range vols {
		n, err := fleetParseBytes(v.Size)
		if err != nil {
			return err
		}
		sizes[i] = n
		need += n
	}

	// A restart of the same task after a host reboot finds its old images (the mounts and
	// mappings are gone). Release them first, so they neither collide nor count against the budget.
	if err := s.Release(ctx, prefix); err != nil {
		return fmt.Errorf("scratch: old volumes of this task are still in use: %v", err)
	}

	s.mu.Lock()
	imgs, err := s.images(ctx)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("scratch: list images: %v", err)
	}
	var used int64
	for _, im := range imgs {
		used += im.Size
	}
	if used+need > s.budget {
		s.mu.Unlock()
		return fmt.Errorf("scratch: budget exceeded on this node: %d bytes in use, %d requested, %d budget", used, need, s.budget)
	}
	created := strconv.FormatInt(s.now().Unix(), 10)
	for i, v := range vols {
		name := prefix + v.Name
		if _, err := s.rbd(ctx, 0, s.scoped("create", "--size", strconv.FormatInt(sizes[i]>>20, 10),
			"--image-feature", "layering,exclusive-lock", name)...); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("scratch: create %s: %v", name, err)
		}
		for k, val := range map[string]string{"fleet.machine": machine, "fleet.created": created} {
			if _, err := s.rbd(ctx, 0, s.scoped("image-meta", "set", name, k, val)...); err != nil {
				s.mu.Unlock()
				return fmt.Errorf("scratch: tag %s: %v", name, err)
			}
		}
	}
	s.mu.Unlock()
	emit(fmt.Sprintf("scratch: created %d volumes", len(vols)))

	for _, v := range vols {
		name := prefix + v.Name
		spec := s.pool + "/" + s.namespace + "/" + name
		out, err := s.rbd(ctx, 0, append(s.auth(), "device", "map", "-o", "exclusive,noshare", spec)...)
		if err != nil {
			return fmt.Errorf("scratch: map %s: %v", name, err)
		}
		dev := strings.TrimSpace(string(out))
		if !strings.HasPrefix(dev, "/dev/rbd") {
			return fmt.Errorf("scratch: map %s returned %q", name, dev)
		}
		mctx, cancel := context.WithTimeout(ctx, fleetScratchMkfsTimeout)
		_, err = s.runner.Run(mctx, "mkfs.ext4", "-q", "-F", "-m", "0", "-L", v.Name,
			"-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1", dev)
		cancel()
		if err != nil {
			return fmt.Errorf("scratch: format %s: %v", name, err)
		}
		dir := filepath.Join(s.mountDir, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := s.mount(dev, dir); err != nil {
			return fmt.Errorf("scratch: mount %s on %s: %v", dev, dir, err)
		}
		for sub, target := range v.Bind {
			p := filepath.Join(dir, sub)
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(p, fleetScratchDirMode(target)); err != nil {
				return err
			}
		}
	}
	emit(fmt.Sprintf("scratch: mapped and mounted %d volumes", len(vols)))
	return nil
}

// WaitMachineGone waits until machined no longer lists the machine. It
// terminates the machine when the wait times out. It reports whether the
// machine is gone.
func (s *fleetScratch) WaitMachineGone(ctx context.Context, machine string, wait time.Duration) bool {
	gone := func() bool {
		_, err := os.Lstat(filepath.Join(s.machinesDir, machine))
		return errors.Is(err, os.ErrNotExist)
	}
	poll := func(d time.Duration) bool {
		deadline := s.now().Add(d)
		for {
			if gone() {
				return true
			}
			if s.now().After(deadline) {
				return false
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	if poll(wait) {
		return true
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, err := s.runner.Run(tctx, "machinectl", "terminate", machine)
	cancel()
	if err != nil {
		s.logger.Warn("scratch: machinectl terminate", "machine", machine, "error", err)
	}
	return poll(15 * time.Second)
}

// fleetRBDDevice is one mapped krbd device.
type fleetRBDDevice struct {
	id, pool, namespace, image string
}

func (s *fleetScratch) devices() ([]fleetRBDDevice, error) {
	ents, err := os.ReadDir(s.sysfsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	read := func(id, f string) string {
		b, _ := os.ReadFile(filepath.Join(s.sysfsDir, id, f))
		return strings.TrimSpace(string(b))
	}
	var out []fleetRBDDevice
	for _, e := range ents {
		id := e.Name()
		if _, err := strconv.Atoi(id); err != nil {
			continue
		}
		out = append(out, fleetRBDDevice{id: id, pool: read(id, "pool"), namespace: read(id, "pool_ns"), image: read(id, "name")})
	}
	return out, nil
}

// Release unmounts, unmaps and removes every volume with the prefix. Each
// step skips what is already gone, so a second call is safe. A volume that is
// still busy stays for the sweeper.
func (s *fleetScratch) Release(ctx context.Context, prefix string) error {
	var errs []string

	ents, err := os.ReadDir(s.mountDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err.Error())
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		dir := filepath.Join(s.mountDir, e.Name())
		if err := s.unmountRetry(dir); err != nil {
			errs = append(errs, fmt.Sprintf("unmount %s: %v", dir, err))
			continue
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err.Error())
		}
	}

	devs, err := s.devices()
	if err != nil {
		errs = append(errs, err.Error())
	}
	busy := map[string]bool{}
	for _, d := range devs {
		if d.pool != s.pool || d.namespace != s.namespace || !strings.HasPrefix(d.image, prefix) {
			continue
		}
		if _, err := s.rbd(ctx, 0, append(s.auth(), "device", "unmap", "/dev/rbd"+d.id)...); err != nil {
			busy[d.image] = true
			errs = append(errs, fmt.Sprintf("unmap %s: %v", d.image, err))
		}
	}

	names, err := s.names(ctx)
	if err != nil {
		errs = append(errs, err.Error())
	}
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) || !fleetScratchImageRe.MatchString(name) || busy[name] {
			continue
		}
		if n, err := s.watchers(ctx, name); err != nil || n > 0 {
			errs = append(errs, fmt.Sprintf("keep %s: %d watchers, %v", name, n, err))
			continue
		}
		if _, err := s.rbd(ctx, 0, s.scoped("rm", "--no-progress", name)...); err != nil {
			errs = append(errs, fmt.Sprintf("remove %s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *fleetScratch) unmountRetry(dir string) error {
	var err error
	for i := 0; i < 5; i++ {
		err = s.unmount(dir)
		if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func (s *fleetScratch) watchers(ctx context.Context, image string) (int, error) {
	out, err := s.rbd(ctx, 0, s.scoped("status", "--format", "json", image)...)
	if err != nil {
		return 0, err
	}
	var st struct {
		Watchers []json.RawMessage `json:"watchers"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, fmt.Errorf("rbd status: %v", err)
	}
	return len(st.Watchers), nil
}

func (s *fleetScratch) imageMeta(ctx context.Context, image, key string) (string, error) {
	out, err := s.rbd(ctx, 0, s.scoped("image-meta", "get", image, key)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Settle ends the in-flight state of a prefix.
func (s *fleetScratch) Settle(prefix string) {
	s.inflightMu.Lock()
	delete(s.inflight, prefix)
	s.inflightMu.Unlock()
}

func (s *fleetScratch) isInflight(prefix string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.inflight[prefix]
}

// Sweep releases every prefix that no live task owns. live reports whether a
// task of this driver owns the prefix.
func (s *fleetScratch) Sweep(ctx context.Context, live func(prefix string) bool) {
	if s.now().Sub(s.startedAt) < s.sweepGrace {
		return
	}
	cands := map[string]string{} // prefix -> one image name
	inPool := map[string]bool{}
	if ents, err := os.ReadDir(s.mountDir); err == nil {
		for _, e := range ents {
			if p := fleetScratchPrefixOf(e.Name()); p != "" {
				cands[p] = e.Name()
			}
		}
	}
	if devs, err := s.devices(); err == nil {
		for _, d := range devs {
			if d.pool == s.pool && d.namespace == s.namespace {
				if p := fleetScratchPrefixOf(d.image); p != "" {
					cands[p] = d.image
				}
			}
		}
	}
	names, err := s.names(ctx)
	if err != nil {
		s.logger.Warn("scratch sweep: list images", "error", err)
	}
	for _, name := range names {
		if p := fleetScratchPrefixOf(name); p != "" {
			cands[p] = name
			inPool[name] = true
		}
	}
	for p, img := range cands {
		if live(p) || s.isInflight(p) {
			continue
		}
		// An image in the pool must prove its age and its machine. Missing or unreadable tags (an rbd
		// error, or a start between create and tag) keep it for the next pass. A prefix with only a
		// mount or a device left (the image is already gone) needs no tags.
		if inPool[img] {
			c, err := s.imageMeta(ctx, img, "fleet.created")
			created, perr := strconv.ParseInt(c, 10, 64)
			if err != nil || perr != nil || s.now().Sub(time.Unix(created, 0)) < s.sweepGrace {
				continue
			}
			m, err := s.imageMeta(ctx, img, "fleet.machine")
			if err != nil || m == "" {
				continue
			}
			if _, err := os.Lstat(filepath.Join(s.machinesDir, m)); err == nil {
				s.logger.Warn("scratch sweep: machine of an orphan prefix still runs", "prefix", p, "machine", m)
				continue
			}
		}
		if err := s.Release(ctx, p); err != nil {
			s.logger.Warn("scratch sweep: release", "prefix", p, "error", err)
		} else {
			s.logger.Info("scratch sweep: released orphan volumes", "prefix", p)
		}
	}
}

// StartSweeper runs Sweep at plugin start (after the grace) and then on
// every interval until ctx ends.
func (s *fleetScratch) StartSweeper(ctx context.Context, live func(prefix string) bool) {
	s.sweepStart.Do(func() {
		go func() {
			t := time.NewTimer(s.sweepGrace + time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s.Sweep(ctx, live)
					t.Reset(s.sweepInterval)
				}
			}
		}()
	})
}
