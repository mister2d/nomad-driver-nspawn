package nspawn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
)

const testAlloc = "0b5c7a1e-2f3d-4c5b-8a9e-1d2c3b4a5f60"

func TestFleetParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"1M": 1 << 20, "40G": 40 << 30, "2T": 2 << 40} {
		if got, err := fleetParseBytes(in); err != nil || got != want {
			t.Errorf("%s: got %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0G", "40", "40g", "1.5G", "40GiB", "-1G"} {
		if _, err := fleetParseBytes(in); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
}

func TestFleetScratchNames(t *testing.T) {
	p := fleetScratchPrefix(testAlloc, "machine")
	img := p + "sys"
	if !fleetScratchImageRe.MatchString(img) {
		t.Fatalf("%s does not match the image pattern", img)
	}
	if got := fleetScratchPrefixOf(img); got != p {
		t.Fatalf("prefix of %s: got %q", img, got)
	}
	for _, bad := range []string{"csi-vol-0b5c7a1e", testAlloc + "-sys", p + "Sys", p + "", "x" + img} {
		if fleetScratchPrefixOf(bad) != "" {
			t.Errorf("%q: want no prefix", bad)
		}
	}
	if fleetScratchPrefix(testAlloc, "a") == fleetScratchPrefix(testAlloc, "b") {
		t.Error("two tasks share a prefix")
	}
}

func scratchCfg(vols ...FleetScratchVolume) *MachineConfig {
	return &MachineConfig{RootMode: fleetRootModeStore, UserNamespacing: true, Scratch: vols}
}

func TestFleetValidateScratch(t *testing.T) {
	good := scratchCfg(
		FleetScratchVolume{Name: "sys", Size: "40G", Bind: map[string]string{".": "/nix/.rw-store", "var-nix": "/nix/var/nix", "tmp": "/tmp"}},
		FleetScratchVolume{Name: "home", Size: "20G", Bind: map[string]string{".": "/home"}},
	)
	if err := fleetValidateScratch(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]*MachineConfig{
		"image mode":  {UserNamespacing: true, Scratch: good.Scratch},
		"no userns":   {RootMode: fleetRootModeStore, Scratch: good.Scratch},
		"name":        scratchCfg(FleetScratchVolume{Name: "Sys", Size: "1G", Bind: map[string]string{".": "/a"}}),
		"dup name":    scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/a"}}, FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/b"}}),
		"small":       scratchCfg(FleetScratchVolume{Name: "a", Size: "512M", Bind: map[string]string{".": "/a"}}),
		"large":       scratchCfg(FleetScratchVolume{Name: "a", Size: "2T", Bind: map[string]string{".": "/a"}}),
		"no bind":     scratchCfg(FleetScratchVolume{Name: "a", Size: "1G"}),
		"abs source":  scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{"/x": "/a"}}),
		"dotdot":      scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{"../x": "/a"}}),
		"unclean":     scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{"x/": "/a"}}),
		"root target": scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/"}}),
		"rel target":  scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "a"}}),
		"colon":       scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/a:rootidmap"}}),
		"comma":       scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{"x,y": "/a"}}),
		"store":       scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/nix/store"}}),
		"secrets":     scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/secrets/x"}}),
		"dup target":  scratchCfg(FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/a"}}, FleetScratchVolume{Name: "b", Size: "1G", Bind: map[string]string{".": "/a"}}),
		"too many": scratchCfg(
			FleetScratchVolume{Name: "a", Size: "1G", Bind: map[string]string{".": "/a"}},
			FleetScratchVolume{Name: "b", Size: "1G", Bind: map[string]string{".": "/b"}},
			FleetScratchVolume{Name: "c", Size: "1G", Bind: map[string]string{".": "/c"}},
			FleetScratchVolume{Name: "d", Size: "1G", Bind: map[string]string{".": "/d"}},
			FleetScratchVolume{Name: "e", Size: "1G", Bind: map[string]string{".": "/e"}}),
	}
	for name, c := range bad {
		if err := fleetValidateScratch(c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFleetScratchBindArgs(t *testing.T) {
	p := fleetScratchPrefix(testAlloc, "machine")
	vols := []FleetScratchVolume{
		{Name: "home", Bind: map[string]string{".": "/home"}},
		{Name: "sys", Bind: map[string]string{".": "/nix/.rw-store", "var-nix": "/nix/var/nix", "tmp": "/tmp"}},
	}
	got := fleetScratchBindArgs(fleetScratchBinds("/run/s", p, vols))
	want := []string{
		"--bind=/run/s/" + p + "sys/tmp:/tmp:idmap",
		"--bind=/run/s/" + p + "home:/home:idmap",
		"--bind=/run/s/" + p + "sys/var-nix:/nix/var/nix:idmap",
		"--bind=/run/s/" + p + "sys:/nix/.rw-store:idmap",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if fleetScratchDirMode("/tmp") != os.ModeSticky|0o777 || fleetScratchDirMode("/home") != 0o755 {
		t.Error("dir modes")
	}
}

func TestFleetStoreLower(t *testing.T) {
	ok := &MachineConfig{RootMode: fleetRootModeStore, StoreLowerPath: "/nix/.ro-store"}
	if err := fleetValidateStoreLower(ok); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/nix/store", "/nix/store/x", "/ro", "nix/.ro", "/nix/.ro/", "/nix/../etc"} {
		if err := fleetValidateStoreLower(&MachineConfig{RootMode: fleetRootModeStore, StoreLowerPath: p}); err == nil {
			t.Errorf("%q: want an error", p)
		}
	}
	if err := fleetValidateStoreLower(&MachineConfig{StoreLowerPath: "/nix/.ro-store"}); err == nil {
		t.Error("image mode: want an error")
	}
	_, tail := fleetStoreRootArgs("/r", "/img", "/nix/store/x-top", "/nix/.ro-store", []string{"--bind=/a:/b:idmap"})
	want := "--bind-ro=/img/nix/store:/nix/store:idmap --bind-ro=/img/nix/store:/nix/.ro-store:idmap --bind=/a:/b:idmap --kill-signal=SIGRTMIN+3 /nix/store/x-top/init"
	if strings.Join(tail, " ") != want {
		t.Fatalf("tail %v", tail)
	}
}

func TestFleetCopyRegistration(t *testing.T) {
	img, root := t.TempDir(), t.TempDir()
	if err := fleetCopyRegistration(img, root); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(img, "nix-path-registration"), []byte("reg"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := fleetCopyRegistration(img, root); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "nix-path-registration"))
	if err != nil || string(b) != "reg" {
		t.Fatalf("copy: %q %v", b, err)
	}
	link := t.TempDir()
	os.Symlink("/etc/passwd", filepath.Join(link, "nix-path-registration"))
	if err := fleetCopyRegistration(link, root); err == nil {
		t.Error("symlink: want an error")
	}
}

func TestNewFleetScratch(t *testing.T) {
	base := FleetScratchConfig{Pool: "sandbox-scratch", Namespace: "prime", ClientID: "nspawn.scratch.prime",
		Keyring: "/etc/ceph/k", Budget: "200G"}
	s, err := newFleetScratch(&base, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	if s.budget != 200<<30 || s.mountDir != fleetScratchDefaultMountDir {
		t.Fatalf("defaults: %+v", s)
	}
	if s, _ := newFleetScratch(nil, nil); s != nil {
		t.Fatal("nil block must give nil")
	}
	for name, mut := range map[string]func(*FleetScratchConfig){
		"pool":     func(c *FleetScratchConfig) { c.Pool = "a b" },
		"ns":       func(c *FleetScratchConfig) { c.Namespace = "" },
		"keyring":  func(c *FleetScratchConfig) { c.Keyring = "k" },
		"budget":   func(c *FleetScratchConfig) { c.Budget = "lots" },
		"mount":    func(c *FleetScratchConfig) { c.MountDir = "/" },
		"interval": func(c *FleetScratchConfig) { c.SweepInterval = "0s" },
		"grace":    func(c *FleetScratchConfig) { c.SweepGrace = "-1h" },
	} {
		c := base
		mut(&c)
		if _, err := newFleetScratch(&c, hclog.NewNullLogger()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// fakeCeph is a fake rbd and mkfs.ext4 with one namespace.
type fakeCeph struct {
	mu       sync.Mutex
	images   map[string]int64
	meta     map[string]map[string]string
	removed  []string
	watchers map[string]int
	calls    []string
	failOn   string
	nextDev  int
	sysfs    string
}

func (f *fakeCeph) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drop the auth and scope flags.
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id", "--keyring", "--pool", "--namespace":
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	call := name + " " + strings.Join(rest, " ")
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.HasPrefix(call, f.failOn) {
		return nil, fmt.Errorf("injected failure: %s", call)
	}
	if name == "mkfs.ext4" || name == "machinectl" {
		return nil, nil
	}
	switch {
	case rest[0] == "ls":
		var parts []string
		for n, s := range f.images {
			parts = append(parts, fmt.Sprintf(`{"image":%q,"size":%d}`, n, s))
		}
		return []byte("[" + strings.Join(parts, ",") + "]"), nil
	case rest[0] == "create":
		var mb int64
		fmt.Sscan(rest[2], &mb)
		f.images[rest[len(rest)-1]] = mb << 20
	case rest[0] == "image-meta" && rest[1] == "set":
		if f.watchers[rest[2]] > 0 {
			// krbd holds the exclusive lock (seen on prime).
			return nil, fmt.Errorf("failed to set metadata: (30) Read-only file system")
		}
		if f.meta[rest[2]] == nil {
			f.meta[rest[2]] = map[string]string{}
		}
		f.meta[rest[2]][rest[3]] = rest[4]
	case rest[0] == "image-meta" && rest[1] == "get":
		v, ok := f.meta[rest[2]][rest[3]]
		if !ok {
			return nil, fmt.Errorf("no key")
		}
		return []byte(v + "\n"), nil
	case rest[0] == "device" && rest[1] == "map":
		spec := strings.Split(rest[len(rest)-1], "/")
		id := fmt.Sprint(f.nextDev)
		f.nextDev++
		d := filepath.Join(f.sysfs, id)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "pool"), []byte(spec[0]+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "pool_ns"), []byte(spec[1]+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "name"), []byte(spec[2]+"\n"), 0o644)
		f.watchers[spec[2]]++
		return []byte("/dev/rbd" + id + "\n"), nil
	case rest[0] == "device" && rest[1] == "unmap":
		id := strings.TrimPrefix(rest[2], "/dev/rbd")
		b, _ := os.ReadFile(filepath.Join(f.sysfs, id, "name"))
		f.watchers[strings.TrimSpace(string(b))]--
		os.RemoveAll(filepath.Join(f.sysfs, id))
	case rest[0] == "status":
		n := f.watchers[rest[len(rest)-1]]
		return []byte(fmt.Sprintf(`{"watchers":[%s]}`, strings.TrimSuffix(strings.Repeat(`{},`, n), ","))), nil
	case rest[0] == "rm":
		img := rest[len(rest)-1]
		delete(f.images, img)
		f.removed = append(f.removed, img)
	}
	return nil, nil
}

func newTestScratch(t *testing.T) (*fleetScratch, *fakeCeph, map[string]bool) {
	t.Helper()
	f := &fakeCeph{images: map[string]int64{}, meta: map[string]map[string]string{}, watchers: map[string]int{}, sysfs: t.TempDir()}
	s, err := newFleetScratch(&FleetScratchConfig{Pool: "sandbox-scratch", Namespace: "prime", ClientID: "c",
		Keyring: "/k", Budget: "100G", MountDir: t.TempDir()}, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	mounted := map[string]bool{}
	s.runner, s.sysfsDir, s.machinesDir = f, f.sysfs, t.TempDir()
	s.mount = func(_, dir string) error { mounted[dir] = true; return nil }
	// After a real unmount the mount point is empty. The subdirectories lived on the volume.
	s.unmount = func(dir string) error {
		delete(mounted, dir)
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return nil
	}
	return s, f, mounted
}

var testVols = []FleetScratchVolume{
	{Name: "sys", Size: "40G", Bind: map[string]string{".": "/nix/.rw-store", "tmp": "/tmp"}},
	{Name: "home", Size: "20G", Bind: map[string]string{".": "/home"}},
}

func TestFleetScratchLifecycle(t *testing.T) {
	s, f, mounted := newTestScratch(t)
	if err := s.Provision(context.Background(), testAlloc, "machine", "machine-"+testAlloc, testVols, func(string) {}); err != nil {
		t.Fatal(err)
	}
	p := fleetScratchPrefix(testAlloc, "machine")
	if len(f.images) != 2 || f.images[p+"sys"] != 40<<30 || len(mounted) != 2 {
		t.Fatalf("images %v mounted %v", f.images, mounted)
	}
	if st, err := os.Stat(filepath.Join(s.mountDir, p+"sys", "tmp")); err != nil || st.Mode()&os.ModeSticky == 0 {
		t.Fatalf("tmp dir: %v %v", st, err)
	}
	if f.meta[p+"sys"]["fleet.machine"] != "machine-"+testAlloc {
		t.Fatalf("meta %v", f.meta)
	}
	// An unrelated image in the namespace counts against the budget but is never touched.
	f.images["unrelated"] = 1 << 30
	if err := s.Release(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(f.images) != 1 || len(mounted) != 0 || len(f.removed) != 2 {
		t.Fatalf("after release: images %v mounted %v removed %v", f.images, mounted, f.removed)
	}
	if err := s.Release(context.Background(), p); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestFleetScratchBudget(t *testing.T) {
	s, f, _ := newTestScratch(t)
	f.images["other"] = 50 << 30
	err := s.Provision(context.Background(), testAlloc, "machine", "m", testVols, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("want a budget error, got %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, " create ") {
			t.Fatalf("created an image over budget: %v", f.calls)
		}
	}
}

func TestFleetScratchFailedStartCleansUp(t *testing.T) {
	s, f, mounted := newTestScratch(t)
	f.failOn = "mkfs.ext4"
	if err := s.Provision(context.Background(), testAlloc, "machine", "m", testVols, func(string) {}); err == nil {
		t.Fatal("want an error")
	}
	if len(f.images) != 0 || len(mounted) != 0 {
		t.Fatalf("left over: images %v mounted %v", f.images, mounted)
	}
	ents, _ := os.ReadDir(f.sysfs)
	if len(ents) != 0 {
		t.Fatalf("devices left mapped: %d", len(ents))
	}
}

func TestFleetScratchReleaseKeepsWatchedImage(t *testing.T) {
	s, f, _ := newTestScratch(t)
	p := fleetScratchPrefix(testAlloc, "machine")
	f.images[p+"sys"] = 1 << 30
	f.watchers[p+"sys"] = 1 // mapped on another host
	if err := s.Release(context.Background(), p); err == nil {
		t.Fatal("want an error for a watched image")
	}
	if len(f.removed) != 0 {
		t.Fatalf("removed a watched image: %v", f.removed)
	}
}

func TestFleetScratchSweep(t *testing.T) {
	s, f, _ := newTestScratch(t)
	live := fleetScratchPrefix(testAlloc, "machine")
	orphan := fleetScratchPrefix("11111111-2222-3333-4444-555555555555", "machine")
	young := fleetScratchPrefix("66666666-2222-3333-4444-555555555555", "machine")
	running := fleetScratchPrefix("77777777-2222-3333-4444-555555555555", "machine")
	old := fmt.Sprint(time.Now().Add(-time.Hour).Unix())
	for _, p := range []string{live, orphan, young, running} {
		f.images[p+"sys"] = 1 << 30
		f.meta[p+"sys"] = map[string]string{"fleet.created": old, "fleet.machine": "m-" + p}
	}
	f.meta[young+"sys"]["fleet.created"] = fmt.Sprint(time.Now().Unix())
	os.WriteFile(filepath.Join(s.machinesDir, "m-"+running), nil, 0o644)
	f.images["csi-vol-x"] = 1 << 30
	untagged := fleetScratchPrefix("88888888-2222-3333-4444-555555555555", "machine")
	f.images[untagged+"sys"] = 1 << 30 // created, not yet tagged

	// Inside the grace period after plugin start nothing happens.
	s.Sweep(context.Background(), func(p string) bool { return p == live })
	if len(f.removed) != 0 {
		t.Fatalf("swept during the grace period: %v", f.removed)
	}
	s.startedAt = time.Now().Add(-time.Hour)
	s.Sweep(context.Background(), func(p string) bool { return p == live })
	if len(f.removed) != 1 || f.removed[0] != orphan+"sys" {
		t.Fatalf("removed %v", f.removed)
	}
}

func TestFleetScratchStaleImagesOfSameTask(t *testing.T) {
	// After a host reboot the old images of the task are in the pool, without mounts or mappings.
	s, f, _ := newTestScratch(t)
	p := fleetScratchPrefix(testAlloc, "machine")
	f.images[p+"sys"] = 40 << 30
	f.images[p+"home"] = 20 << 30
	f.images["other"] = 40 << 30 // 100G budget: 60 + 40 + 60 would exceed it without the release
	if err := s.Provision(context.Background(), testAlloc, "machine", "m", testVols, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 2 || len(f.images) != 3 {
		t.Fatalf("removed %v images %v", f.removed, f.images)
	}
}

func TestFleetScratchTaskConfigDecode(t *testing.T) {
	cfg := map[string]interface{}{
		"image":     "/var/lib/machines/x",
		"root_mode": "store",
		"scratch": []interface{}{
			map[string]interface{}{"name": "sys", "size": "40G",
				"bind": []interface{}{map[string]interface{}{".": "/nix/.rw-store", "tmp": "/tmp"}}},
			map[string]interface{}{"name": "home", "size": "20G",
				"bind": []interface{}{map[string]interface{}{".": "/home"}}},
		},
		"store_lower_path": "/nix/.ro-store",
	}
	var c MachineConfig
	hclutils.NewConfigParser(taskConfigSpec).ParseJson(t, mustJSON(t, map[string]interface{}{"Config": cfg}), &c)
	if len(c.Scratch) != 2 || c.Scratch[0].Bind["tmp"] != "/tmp" || c.Scratch[1].Bind["."] != "/home" || c.StoreLowerPath != "/nix/.ro-store" {
		t.Fatalf("decoded %+v", c)
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFleetScratchSweepSkipsInflight(t *testing.T) {
	s, f, _ := newTestScratch(t)
	s.startedAt = time.Now().Add(-time.Hour)
	if err := s.Provision(context.Background(), testAlloc, "machine", "m", testVols, func(string) {}); err != nil {
		t.Fatal(err)
	}
	p := fleetScratchPrefix(testAlloc, "machine")
	// Old tags, no live task, no machine: only the in-flight mark protects the start.
	for _, v := range testVols {
		f.meta[p+v.Name]["fleet.created"] = fmt.Sprint(time.Now().Add(-time.Hour).Unix())
	}
	s.Sweep(context.Background(), func(string) bool { return false })
	if len(f.removed) != 0 {
		t.Fatalf("swept an in-flight start: %v", f.removed)
	}
	s.Settle(p)
	s.Sweep(context.Background(), func(string) bool { return false })
	if len(f.removed) != 2 {
		t.Fatalf("after settle: removed %v", f.removed)
	}
}

func TestFleetScratchFailedStartSettles(t *testing.T) {
	s, f, _ := newTestScratch(t)
	f.failOn = "mkfs.ext4"
	s.Provision(context.Background(), testAlloc, "machine", "m", testVols, func(string) {})
	if s.isInflight(fleetScratchPrefix(testAlloc, "machine")) {
		t.Fatal("a failed start stays in flight")
	}
}
