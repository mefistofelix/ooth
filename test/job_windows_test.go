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

	"golang.org/x/sys/windows"
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
	descriptor, err := windows.GetSecurityInfo(windows.Handle(handle), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	return owner.String(), socketPermissionSDDL(t, descriptor)
}

func socketPermissionSDDL(t *testing.T, descriptor *windows.SECURITY_DESCRIPTOR) string {
	t.Helper()
	// Windows may record automatic inheritance during SetSecurityInfo. Compare
	// owner, every ACE and DACL protection, not this inheritance-history bit.
	if err := descriptor.SetControl(windows.SE_DACL_AUTO_INHERITED, 0); err != nil {
		t.Fatal(err)
	}
	// String() includes every available field, even ones GetSecurityInfo was
	// not asked to retrieve. Request only the owner and DACL being tested.
	var text *uint16
	convert := jobSecurity.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	information := windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	if ok, _, err := convert.Call(uintptr(unsafe.Pointer(descriptor)), 1, uintptr(information), uintptr(unsafe.Pointer(&text)), 0); ok == 0 {
		t.Fatal(err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(text)))
	return windows.UTF16PtrToString(text)
}

func TestSocketPermissionComparison(t *testing.T) {
	baseline := "O:SYD:P(A;;FA;;;SY)"
	for _, test := range []struct {
		sddl  string
		equal bool
	}{
		{"O:SYG:BAD:PAI(A;;FA;;;SY)", true},
		{"O:BAD:P(A;;FA;;;SY)", false},
		{"O:SYD:P(A;;FR;;;SY)", false},
		{"O:SYD:(A;;FA;;;SY)", false},
		{"O:SYD:P(A;;FA;;;SY)(A;;FA;;;WD)", false},
	} {
		descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
		if err != nil {
			t.Fatal(err)
		}
		if equal := socketPermissionSDDL(t, descriptor) == baseline; equal != test.equal {
			t.Errorf("comparison of %s: equal=%v, want %v", test.sddl, equal, test.equal)
		}
	}
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
	// Let Windows canonicalize well-known SIDs, e.g. Administrator becomes LA.
	expected, err := windows.SecurityDescriptorFromString("O:" + account.Uid + "D:P(A;;FA;;;SY)(A;;FA;;;" + account.Uid + ")")
	if err != nil {
		t.Fatal(err)
	}
	if owner != account.Uid || descriptor != expected.String() {
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
