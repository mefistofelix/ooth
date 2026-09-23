package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func treeOwnershipAvailable(*processOwner) bool { return true }

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	handle, err := syscall.OpenProcess(0x100000, false, uint32(pid))
	if err == syscall.Errno(87) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	state, err := syscall.WaitForSingleObject(handle, 4000)
	if err != nil || state != syscall.WAIT_OBJECT_0 {
		t.Fatalf("process %d is still alive: state=%d error=%v", pid, state, err)
	}
}

func TestJobAssignmentFailureDoesNotRun(t *testing.T) {
	directory := t.TempDir()
	cmd := treeCommand(t, directory, true)
	cmd.SysProcAttr = &syscall.SysProcAttr{JobObjects: []syscall.Handle{syscall.InvalidHandle}}
	if err := cmd.Start(); err == nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("started despite invalid job handle")
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("process executed before job assignment: %v %v", entries, err)
	}
}

func TestJobForbidsBreakaway(t *testing.T) {
	if os.Getenv("TEST_JOB_BREAKAWAY") == "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x01000000} // CREATE_BREAKAWAY_FROM_JOB
		if err := cmd.Run(); err == nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestJobForbidsBreakaway$")
	cmd.Env = append(os.Environ(), "TEST_JOB_BREAKAWAY=1")
	job, err := newProcessJob(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestJobOwnerExitKillsFamily(t *testing.T) {
	if os.Getenv("TEST_JOB_OWNER") == "1" {
		directory := os.Getenv("TEST_TREE_DIRECTORY")
		cmd := treeCommand(t, directory, false)
		job, err := newProcessJob(cmd, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			job.Close()
			t.Fatal(err)
		}
		treePID(t, filepath.Join(directory, "leaf"))
		// Intentionally bypass Close: the kernel must close our private handle.
		os.Exit(0)
	}
	directory := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJobOwnerExitKillsFamily$")
	cmd.Env = append(os.Environ(), "TEST_JOB_OWNER=1", "TEST_TREE_DIRECTORY="+directory)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"root", "middle", "leaf"} {
		assertProcessGone(t, treePID(t, filepath.Join(directory, stage)))
	}
}
