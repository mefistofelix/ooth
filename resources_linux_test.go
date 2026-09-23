package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestResourceCgroupPaths(t *testing.T) {
	paths := resourceGroupPaths("0::/container/app\n", "1 2 0:1 /container /sys/fs/cgroup rw - cgroup2 cgroup rw\n", "/sys/fs/cgroup/delegation/ooth")
	want := []string{"/sys/fs/cgroup/app", "/sys/fs/cgroup", "/sys/fs/cgroup/delegation/ooth", "/sys/fs/cgroup/delegation"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("%v", paths)
	}
	paths = resourceGroupPaths("0::/\n", "1 2 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n", "")
	if !reflect.DeepEqual(paths, []string{"/sys/fs/cgroup"}) {
		t.Fatal(paths)
	}
}

func TestResourceCgroupLimits(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cpu.max"), "50000 100000\n")
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1250000\nuser_usec 1000000\n")
	write(t, filepath.Join(dir, "cpuset.cpus.effective"), "0-3,6\n")
	write(t, filepath.Join(dir, "memory.max"), "1000\n")
	write(t, filepath.Join(dir, "memory.current"), "950\n")
	group, err := readResourceGroup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if group.cores != .5 || group.cpuSeconds != 1.25 || group.availableMemory != 5 {
		t.Fatalf("%+v", group)
	}
	write(t, filepath.Join(dir, "cpu.max"), "max 100000\n")
	write(t, filepath.Join(dir, "memory.current"), "1100\n")
	group, err = readResourceGroup(dir)
	if err != nil || group.cores != 5 || group.availableMemory != 0 {
		t.Fatalf("%+v %v", group, err)
	}
	write(t, filepath.Join(dir, "memory.max"), "max\n")
	group, err = readResourceGroup(dir)
	if err != nil || group.availableMemory != 100 {
		t.Fatalf("%+v %v", group, err)
	}
	write(t, filepath.Join(dir, "cpu.stat"), "")
	if _, err := readResourceGroup(dir); err == nil {
		t.Fatal("accepted missing counter for a limited group")
	}
}
