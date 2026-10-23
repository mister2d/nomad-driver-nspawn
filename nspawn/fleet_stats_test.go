package nspawn

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFleetScopeCgroupPath(t *testing.T) {
	got, err := scopeCgroupPath("0::/machine.slice/machine-web\\x2dabc.scope/init.scope\n")
	if err != nil || got != "/machine.slice/machine-web\\x2dabc.scope" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := scopeCgroupPath("0::/system.slice/nomad.service\n"); err == nil {
		t.Fatal("expected error outside a machine scope")
	}
	if _, err := scopeCgroupPath("1:cpu:/foo\n"); err == nil {
		t.Fatal("expected error without a v2 entry")
	}
}

func writeScope(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFleetCollectScopeStats(t *testing.T) {
	dir := t.TempDir()
	writeScope(t, dir, map[string]string{
		"memory.current": "5000\n",
		"memory.peak":    "9000\n",
		"memory.stat":    "anon 1000\nfile 2000\nfile_mapped 300\nkernel 400\n",
		"cpu.stat":       "usage_usec 1000000\nuser_usec 600000\nsystem_usec 400000\nnr_throttled 2\nthrottled_usec 50\n",
	})
	t0 := time.Unix(1000, 0)
	u, s1, err := collectScopeStats(dir, cpuSample{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	m := u.ResourceUsage.MemoryStats
	if m.Usage != 5000 || m.MaxUsage != 9000 || m.RSS != 1000 || m.Cache != 2000 ||
		m.MappedFile != 300 || m.KernelUsage != 400 || m.Swap != 0 {
		t.Fatalf("memory: %+v", m)
	}
	c := u.ResourceUsage.CpuStats
	if c.Percent != 0 || c.ThrottledPeriods != 2 || c.ThrottledTime != 50000 {
		t.Fatalf("first cpu sample: %+v", c)
	}

	writeScope(t, dir, map[string]string{
		"cpu.stat": "usage_usec 2000000\nuser_usec 1100000\nsystem_usec 900000\n",
	})
	u, _, err = collectScopeStats(dir, s1, t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	c = u.ResourceUsage.CpuStats
	if c.Percent != 50 || c.UserMode != 25 || c.SystemMode != 25 {
		t.Fatalf("cpu delta: %+v", c)
	}
}

func TestFleetCollectScopeStatsMissing(t *testing.T) {
	if _, _, err := collectScopeStats(t.TempDir(), cpuSample{}, time.Now()); err == nil {
		t.Fatal("expected error without cpu.stat")
	}
}

func TestFleetScopeCgroupDirNoProcess(t *testing.T) {
	if _, err := scopeCgroupDir(t.TempDir(), 4294967295); err == nil {
		t.Fatal("expected error for missing pid")
	}
}

func TestFleetTicks(t *testing.T) {
	cases := []struct {
		percent, total float64
		ncpu           int
		want           float64
	}{
		{100, 8000, 8, 1000}, // one full core of an 8 x 1000 MHz node
		{250, 19200, 8, 6000},
		{50, 0, 8, 0}, // unknown node compute
		{50, 8000, 0, 0},
	}
	for _, c := range cases {
		if got := fleetTicks(c.percent, c.total, c.ncpu); got != c.want {
			t.Errorf("fleetTicks(%v, %v, %d) = %v, want %v", c.percent, c.total, c.ncpu, got, c.want)
		}
	}
}
