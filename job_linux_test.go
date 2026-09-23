package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func treeOwnershipAvailable(owner *processOwner) bool { return owner.root != "" }

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists (possibly an unreaped zombie)", pid)
}

func TestCgroupUnavailableWarnsAndRuns(t *testing.T) {
	var output bytes.Buffer
	owner, err := newProcessOwner(filepath.Join(t.TempDir(), "missing"), slog.New(slog.NewTextHandler(&output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.root != "" || !strings.Contains(output.String(), "descendant supervision disabled") {
		t.Fatalf("missing explicit fallback warning: %s", output.String())
	}
	cmd := treeCommand(t, t.TempDir(), false)
	// Use a helper with no descendants, since this mode intentionally lacks ownership.
	cmd.Args = []string{cmd.Path, "-test.run=^$"}
	job, err := owner.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(job); err != nil {
		t.Fatal(err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCgroupPlacementFallback(t *testing.T) {
	root := os.Getenv("OOTH_TEST_UNDELEGATED_GROUP")
	if root == "" {
		t.Skip("set OOTH_TEST_UNDELEGATED_GROUP to a writable cgroup outside the user's migration delegation")
	}
	var output bytes.Buffer
	owner, err := newProcessOwner(root, slog.New(slog.NewTextHandler(&output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.root == "" {
		t.Fatal("fixture requires a writable cgroup with migration denied")
	}
	cmd := treeCommand(t, t.TempDir(), false)
	cmd.Args = []string{cmd.Path, "-test.run=^$"}
	job, err := owner.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if err := owner.Wait(job); err != nil {
		t.Fatal(err)
	}
	if !owner.groupUnavailable || !strings.Contains(output.String(), "cgroup placement unavailable") {
		t.Fatalf("missing placement fallback: %s", output.String())
	}
}

func TestSubreaperWithoutCgroup(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("this case verifies adoption outside PID 1")
	}
	var previous int32
	if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 37, uintptr(unsafe.Pointer(&previous)), 0, 0, 0, 0); err != 0 {
		t.Fatal(err)
	}
	owner, err := newProcessOwner(filepath.Join(t.TempDir(), "missing"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		owner.Close()
		var restored int32
		_, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 37, uintptr(unsafe.Pointer(&restored)), 0, 0, 0, 0)
		if err != 0 || restored != previous {
			t.Errorf("subreaper state not restored: %d, want %d; error=%v", restored, previous, err)
		}
	}()
	if owner.root != "" || owner.signals == nil {
		t.Fatal("subreaper must remain active without cgroups")
	}
	directory := t.TempDir()
	job, err := owner.Start(treeCommand(t, directory, true))
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if err := owner.Wait(job); err != nil {
		t.Fatal(err)
	}
	leaf := treePID(t, filepath.Join(directory, "leaf"))
	process, err := os.FindProcess(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	defer process.Kill()
	// Both ancestors have exited; the living grandchild must now belong to us.
	deadline := time.Now().Add(4 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(leaf), "status"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), fmt.Sprintf("PPid:\t%d\n", os.Getpid())) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orphan was not adopted by this non-PID-1 supervisor")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Do not Wait here: SIGCHLD handling in the owner must reap the orphan.
	assertProcessGone(t, leaf)
	assertProcessGone(t, treePID(t, filepath.Join(directory, "middle")))
}
