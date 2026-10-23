package nspawn

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// cgroupV2Root is the mount point of the unified cgroup hierarchy.
const cgroupV2Root = "/sys/fs/cgroup"

var (
	fleetScopeMemMeasured = []string{"RSS", "Cache", "Swap", "Usage", "Max Usage", "Kernel Usage"}
	fleetScopeCPUMeasured = []string{"System Mode", "User Mode", "Throttled Periods", "Throttled Time", "Percent"}
)

// scopeCgroupPath extracts the machine scope cgroup path from the content of
// /proc/<pid>/cgroup. The leader of a booted machine can live in a child
// cgroup such as init.scope, so the path is cut after the machine scope.
func scopeCgroupPath(procCgroup string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(procCgroup))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(line, "0::"), "/")
		for i, p := range parts {
			if strings.HasPrefix(p, "machine-") && strings.HasSuffix(p, ".scope") {
				return strings.Join(parts[:i+1], "/"), nil
			}
		}
		return "", fmt.Errorf("pid is not in a machine scope: %q", line)
	}
	return "", fmt.Errorf("no cgroup v2 entry found")
}

// scopeCgroupDir returns the absolute cgroup directory of the machine scope
// that holds the process pid. It returns an error when cgroup v2 is not in use
// or the scope cannot be found.
func scopeCgroupDir(root string, pid uint32) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	rel, err := scopeCgroupPath(string(data))
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, rel)
	if _, err := os.Stat(filepath.Join(dir, "cpu.stat")); err != nil {
		return "", err
	}
	return dir, nil
}

// parseFlatKeyed parses cgroup files with "key value" lines.
func parseFlatKeyed(b []byte) map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(string(bytes.TrimSpace(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			out[f[0]] = v
		}
	}
	return out
}

// parseCgroupUint parses a single-value cgroup file. The value "max" is
// reported as an error so callers treat it as not measured.
func parseCgroupUint(b []byte) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// cpuSample is one reading of cpu.stat.
type cpuSample struct {
	at         time.Time
	usageUsec  uint64
	userUsec   uint64
	systemUsec uint64
	valid      bool
}

func readUintFile(dir, name string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, false
	}
	v, err := parseCgroupUint(b)
	return v, err == nil
}

// collectScopeStats reads the scope cgroup in dir. The previous CPU sample
// gives the CPU percentage. The first call reports 0 percent.
func collectScopeStats(dir string, prev cpuSample, now time.Time) (*drivers.TaskResourceUsage, cpuSample, error) {
	cpuRaw, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return nil, prev, err
	}
	cpu := parseFlatKeyed(cpuRaw)
	cur := cpuSample{
		at:         now,
		usageUsec:  cpu["usage_usec"],
		userUsec:   cpu["user_usec"],
		systemUsec: cpu["system_usec"],
		valid:      true,
	}

	mem := &cstructs.MemoryStats{Measured: fleetScopeMemMeasured}
	if v, ok := readUintFile(dir, "memory.current"); ok {
		mem.Usage = v
	}
	if v, ok := readUintFile(dir, "memory.peak"); ok {
		mem.MaxUsage = v
	}
	if v, ok := readUintFile(dir, "memory.swap.current"); ok {
		mem.Swap = v
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		ms := parseFlatKeyed(raw)
		mem.RSS = ms["anon"]
		mem.Cache = ms["file"]
		mem.MappedFile = ms["file_mapped"]
		mem.KernelUsage = ms["kernel"]
		if mem.KernelUsage == 0 {
			mem.KernelUsage = ms["kernel_stack"] + ms["slab"]
		}
	}

	cs := &cstructs.CpuStats{
		ThrottledPeriods: cpu["nr_throttled"],
		ThrottledTime:    cpu["throttled_usec"] * 1000,
		Measured:         fleetScopeCPUMeasured,
	}
	if prev.valid && now.After(prev.at) &&
		cur.usageUsec >= prev.usageUsec {
		elapsed := float64(now.Sub(prev.at).Microseconds())
		if elapsed > 0 {
			cs.UserMode = float64(cur.userUsec-prev.userUsec) / elapsed * 100
			cs.SystemMode = float64(cur.systemUsec-prev.systemUsec) / elapsed * 100
			cs.Percent = float64(cur.usageUsec-prev.usageUsec) / elapsed * 100
		}
	}

	return &drivers.TaskResourceUsage{
		ResourceUsage: &cstructs.ResourceUsage{MemoryStats: mem, CpuStats: cs},
		Timestamp:     now.UnixNano(),
	}, cur, nil
}

// streamScopeStats sends scope statistics on ch until ctx is done or the
// scope cgroup disappears (the task ended).
func streamScopeStats(ctx context.Context, dir string, interval time.Duration, ch chan<- *drivers.TaskResourceUsage) {
	defer close(ch)
	timer := time.NewTimer(0)
	defer timer.Stop()
	var prev cpuSample
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(interval)
		}
		usage, cur, err := collectScopeStats(dir, prev, time.Now())
		if err != nil {
			return
		}
		prev = cur
		select {
		case <-ctx.Done():
			return
		case ch <- usage:
		}
	}
}
