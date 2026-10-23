package nspawn

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

func TestFleetCPUWeightConversion(t *testing.T) {
	cases := []struct {
		shares int64
		want   int64
		ok     bool
	}{
		{0, 0, false},
		{1, 0, false},
		{2, 1, true},
		{1024, 39, true},
		{262144, 10000, true},
		{999999, 10000, true},
	}
	for _, c := range cases {
		got, ok := fleetCPUSharesToWeight(c.shares)
		if ok != c.ok || got != c.want {
			t.Errorf("shares %d: got %d,%v want %d,%v", c.shares, got, ok, c.want, c.ok)
		}
	}
}

func TestFleetCPUCpusetValidation(t *testing.T) {
	for _, s := range []string{"0", "0-3", "0-3,6", "1,3,5-7"} {
		if !fleetValidCpuset(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "a", "0-", "-1", "0,,1", "0-3;rm", "1 2"} {
		if fleetValidCpuset(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestFleetCPUQuotaPercent(t *testing.T) {
	if p, ok := fleetCPUQuotaPercent(&drivers.LinuxResources{CPUQuota: 200000, CPUPeriod: 100000}, 8); !ok || p != 200 {
		t.Errorf("quota/period: %d %v", p, ok)
	}
	if p, ok := fleetCPUQuotaPercent(&drivers.LinuxResources{PercentTicks: 0.25}, 8); !ok || p != 200 {
		t.Errorf("percent ticks: %d %v", p, ok)
	}
	if p, ok := fleetCPUQuotaPercent(&drivers.LinuxResources{PercentTicks: 0.001}, 8); !ok || p != 1 {
		t.Errorf("minimum: %d %v", p, ok)
	}
	if _, ok := fleetCPUQuotaPercent(&drivers.LinuxResources{}, 8); ok {
		t.Error("empty resources must give no quota")
	}
	if _, ok := fleetCPUQuotaPercent(nil, 8); ok {
		t.Error("nil resources must give no quota")
	}
}

func fleetTestResources(shares int64, cpuset string, ticks float64) *drivers.Resources {
	return &drivers.Resources{
		NomadResources: &structs.AllocatedTaskResources{
			Memory: structs.AllocatedMemoryResources{MemoryMB: 512},
		},
		LinuxResources: &drivers.LinuxResources{CPUShares: shares, CpusetCpus: cpuset, PercentTicks: ticks},
	}
}

func TestFleetCPUResourceProperties(t *testing.T) {
	props := map[string]string{}
	_, err := fleetResourceProperties(props, fleetResourceInput{Resources: fleetTestResources(1024, "0-1", 0.5), NumCPU: 8})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"MemoryMax": "536870912", "CPUWeight": "39", "AllowedCPUs": "0-1"}
	if len(props) != len(want) {
		t.Fatalf("got %v", props)
	}
	for k, v := range want {
		if props[k] != v {
			t.Errorf("%s: got %q want %q", k, props[k], v)
		}
	}
}

func TestFleetCPUHardLimitOptIn(t *testing.T) {
	res := fleetTestResources(1024, "", 0.25)
	props := map[string]string{}
	if _, err := fleetResourceProperties(props, fleetResourceInput{Resources: res, NumCPU: 8}); err != nil {
		t.Fatal(err)
	}
	if _, ok := props["CPUQuota"]; ok {
		t.Fatal("quota must be off by default")
	}
	for name, in := range map[string]fleetResourceInput{
		"task":   {Resources: res, NumCPU: 8, CPUHardLimit: true},
		"plugin": {Resources: res, NumCPU: 8, ForceCPUHardLimit: true},
	} {
		props = map[string]string{}
		if _, err := fleetResourceProperties(props, in); err != nil {
			t.Fatal(err)
		}
		if props["CPUQuota"] != "200%" {
			t.Errorf("%s: CPUQuota %q", name, props["CPUQuota"])
		}
	}
}

func TestFleetCPUMemoryHighAndKeepsUserKeys(t *testing.T) {
	res := fleetTestResources(0, "", 0)
	res.NomadResources.Memory.MemoryMaxMB = 1024
	props := map[string]string{"CPUWeight": "7", "MemoryMax": "1"}
	if _, err := fleetResourceProperties(props, fleetResourceInput{Resources: res}); err != nil {
		t.Fatal(err)
	}
	if props["MemoryHigh"] != "536870912" || props["MemoryMax"] != "1" {
		t.Fatalf("got %v", props)
	}
}

func TestFleetCPUInvalidCpusetRejected(t *testing.T) {
	_, err := fleetResourceProperties(map[string]string{}, fleetResourceInput{Resources: fleetTestResources(0, "0;x", 0)})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFleetCPUArgsSortedAndRendered(t *testing.T) {
	c := MachineConfig{Image: "x", imagePath: t.TempDir(), Properties: map[string]string{
		"MemoryMax": "1", "CPUWeight": "39", "AllowedCPUs": "0-1", "CPUQuota": "200%",
	}}
	args, err := c.ConfigArray()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range args {
		if strings.HasPrefix(a, "--property=") {
			got = append(got, a)
		}
	}
	want := "--property=AllowedCPUs=0-1 --property=CPUQuota=200% --property=CPUWeight=39 --property=MemoryMax=1"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
}
