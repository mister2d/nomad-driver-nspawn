package nspawn

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// fleetCPUWeightMin and fleetCPUWeightMax are the limits of the systemd
// CPUWeight property.
const (
	fleetCPUWeightMin = 1
	fleetCPUWeightMax = 10000
)

// fleetCPUSharesToWeight converts cgroup v1 CPU shares (2..262144) to a
// cgroup v2 weight (1..10000). This is the formula that runc and systemd use:
// weight = 1 + ((shares-2)*9999)/262142. It returns false when shares carry
// no usable value.
func fleetCPUSharesToWeight(shares int64) (int64, bool) {
	if shares < 2 {
		return 0, false
	}
	if shares > 262144 {
		shares = 262144
	}
	w := 1 + ((shares-2)*9999)/262142
	if w < fleetCPUWeightMin {
		w = fleetCPUWeightMin
	}
	if w > fleetCPUWeightMax {
		w = fleetCPUWeightMax
	}
	return w, true
}

var fleetCpusetRe = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

// fleetValidCpuset reports whether s is a cpuset list such as "0-3,6".
func fleetValidCpuset(s string) bool {
	return fleetCpusetRe.MatchString(s)
}

// fleetCPUQuotaPercent computes the CPUQuota percentage (percent of one CPU,
// so 200 means two full CPUs) for the hard limit. Sources, in order:
//
//  1. LinuxResources.CPUQuota and CPUPeriod, when both are positive:
//     percent = quota * 100 / period.
//  2. LinuxResources.PercentTicks, the share of the whole node that the task
//     reserved (0..1): percent = PercentTicks * numCPU * 100.
//
// The result is rounded up and is at least 1. It returns false when Nomad
// gave no usable value. The SDK documents both fields as docker oriented,
// so they can be zero for this driver.
func fleetCPUQuotaPercent(lr *drivers.LinuxResources, numCPU int) (int64, bool) {
	if lr == nil {
		return 0, false
	}
	if lr.CPUQuota > 0 && lr.CPUPeriod > 0 {
		p := (lr.CPUQuota*100 + lr.CPUPeriod - 1) / lr.CPUPeriod
		if p < 1 {
			p = 1
		}
		return p, true
	}
	if lr.PercentTicks > 0 && numCPU > 0 {
		f := lr.PercentTicks * float64(numCPU) * 100
		p := int64(f)
		if float64(p) < f {
			p++
		}
		if p < 1 {
			p = 1
		}
		return p, true
	}
	return 0, false
}

// fleetResourceInput holds everything that decides the scope properties.
type fleetResourceInput struct {
	Resources *drivers.Resources
	// CPUHardLimit is the task option cpu_hard_limit.
	CPUHardLimit bool
	// ForceCPUHardLimit is the plugin option cpu_hard_limit.
	ForceCPUHardLimit bool
	// NumCPU is the CPU count of the node.
	NumCPU int
}

// fleetResourceProperties maps Nomad task resources to systemd scope
// properties. Keys already present in props are kept. The function returns
// the keys it set.
func fleetResourceProperties(props map[string]string, in fleetResourceInput) ([]string, error) {
	var set []string
	setDefault := func(k, v string) {
		if _, ok := props[k]; ok {
			return
		}
		props[k] = v
		set = append(set, k)
	}
	res := in.Resources
	if res == nil {
		return set, nil
	}

	if nr := res.NomadResources; nr != nil {
		if nr.Memory.MemoryMaxMB != 0 {
			setDefault("MemoryHigh", strconv.Itoa(int(nr.Memory.MemoryMB*1024*1024)))
			setDefault("MemoryMax", strconv.Itoa(int(nr.Memory.MemoryMaxMB*1024*1024)))
		} else {
			setDefault("MemoryMax", strconv.Itoa(int(nr.Memory.MemoryMB*1024*1024)))
		}
	}

	if lr := res.LinuxResources; lr != nil {
		if w, ok := fleetCPUSharesToWeight(lr.CPUShares); ok {
			setDefault("CPUWeight", strconv.FormatInt(w, 10))
		}
		if lr.CpusetCpus != "" {
			if !fleetValidCpuset(lr.CpusetCpus) {
				return set, fmt.Errorf("invalid cpuset %q from Nomad", lr.CpusetCpus)
			}
			setDefault("AllowedCPUs", lr.CpusetCpus)
		}
		if in.CPUHardLimit || in.ForceCPUHardLimit {
			n := in.NumCPU
			if n <= 0 {
				n = runtime.NumCPU()
			}
			if p, ok := fleetCPUQuotaPercent(lr, n); ok {
				setDefault("CPUQuota", strconv.FormatInt(p, 10)+"%")
			}
		}
	}
	return set, nil
}
