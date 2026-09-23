package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
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

func TestIdentityWindowsLogonFailure(t *testing.T) {
	password := "ooth-example-password-for-error-test"
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	release, err := (Identity{User: "ooth-missing-account-" + t.Name(), Password: &password}).apply(cmd)
	if release != nil {
		release()
	}
	if err == nil || strings.Contains(err.Error(), password) || cmd.Process != nil {
		t.Fatal("failed logon was not rejected safely")
	}
}

func TestIdentityDifferentWindowsUser(t *testing.T) {
	username := os.Getenv("OOTH_TEST_WINDOWS_USER")
	password, supplied := os.LookupEnv("OOTH_TEST_WINDOWS_PASSWORD")
	if username == "" || !supplied {
		t.Skip("set OOTH_TEST_WINDOWS_USER and OOTH_TEST_WINDOWS_PASSWORD in a suitably privileged test session")
	}
	account, err := user.Lookup(username)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"tcp", "unix"} {
		reply := runIdentityWorker(t, executable, Identity{User: username, Password: &password}, network)
		if reply["uid"] != account.Uid {
			t.Fatalf("worker SID %s, expected %s", reply["uid"], account.Uid)
		}
	}
}

func socketSecurity(t *testing.T, path string) (string, string) {
	t.Helper()
	name, _ := syscall.UTF16PtrFromString(path)
	handle, err := syscall.CreateFile(name, 0x20000, 7, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	var descriptor unsafe.Pointer
	var owner *syscall.SID
	if code, _, _ := getSecurityInfo.Call(uintptr(handle), 1, 5, uintptr(unsafe.Pointer(&owner)), 0, 0, 0, uintptr(unsafe.Pointer(&descriptor))); code != 0 {
		t.Fatal(syscall.Errno(code))
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(descriptor)))
	sid, err := owner.String()
	if err != nil {
		t.Fatal(err)
	}
	var text *uint16
	var size uint32
	convert := jobSecurity.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	if ok, _, err := convert.Call(uintptr(descriptor), 1, 5, uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&size))); ok == 0 {
		t.Fatal(err)
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(text))))
	return sid, syscall.UTF16ToString(unsafe.Slice(text, size))
}

func TestSocketIdentityWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := OpenListener("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	previousOwner, previous := socketSecurity(t, path)
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	finish, err := (Identity{User: account.Username}).socket(path, 0660)
	if err != nil {
		t.Fatal(err)
	}
	owner, descriptor := socketSecurity(t, path)
	if owner != account.Uid || !strings.Contains(descriptor, "D:P") || !strings.Contains(descriptor, "(A;;FA;;;"+account.Uid+")") || strings.Contains(descriptor, ";;;WD)") || strings.Contains(descriptor, ";;;BU)") {
		t.Errorf("unexpected socket owner/ACL: %s %s", owner, descriptor)
	}
	if err := finish(false); err != nil {
		t.Fatal(err)
	}
	restoredOwner, restored := socketSecurity(t, path)
	if restoredOwner != previousOwner || restored != previous {
		t.Fatalf("security rollback: before %s; after %s", previous, restored)
	}
}
