package nspawn

import "testing"

func TestFleetTasksMaxPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		task, def int64
		want      string
	}{
		{"unset", 0, 0, ""},
		{"default only", 0, 4096, "4096"},
		{"task only", 512, 0, "512"},
		{"lower task wins", 512, 4096, "512"},
		{"task above ceiling is capped", 4000000, 4096, "4096"},
		{"task equal to ceiling", 4096, 4096, "4096"},
		{"no ceiling when default is 0", 4000000, 0, "4000000"},
	}
	for _, c := range cases {
		props := map[string]string{}
		set, err := fleetResourceProperties(props, fleetResourceInput{TasksMax: c.task, DefaultTasksMax: c.def})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if props["TasksMax"] != c.want {
			t.Errorf("%s: got %q want %q", c.name, props["TasksMax"], c.want)
		}
		if (c.want == "") != (len(set) == 0) {
			t.Errorf("%s: set list %v", c.name, set)
		}
	}
}

func TestFleetTasksMaxNegativeRejected(t *testing.T) {
	if _, err := fleetResourceProperties(map[string]string{}, fleetResourceInput{TasksMax: -1}); err == nil {
		t.Error("negative tasks_max must fail")
	}
	if _, err := fleetResourceProperties(map[string]string{}, fleetResourceInput{DefaultTasksMax: -5}); err == nil {
		t.Error("negative default_tasks_max must fail")
	}
}

func TestFleetTasksMaxWithResourcesAndArgs(t *testing.T) {
	props := map[string]string{}
	if _, err := fleetResourceProperties(props, fleetResourceInput{
		Resources: fleetTestResources(1024, "", 0), TasksMax: 2048,
	}); err != nil {
		t.Fatal(err)
	}
	c := MachineConfig{Image: "x", imagePath: t.TempDir(), Properties: props}
	args, err := c.ConfigArray()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range args {
		if a == "--property=TasksMax=2048" {
			found = true
		}
	}
	if !found {
		t.Fatalf("TasksMax argument missing: %v", args)
	}
}
