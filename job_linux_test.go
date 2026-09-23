package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
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

func TestIdentityDifferentLinuxUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("run as root to verify dropping to another UID/GID")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId("1")
	if err != nil {
		t.Fatal(err)
	}
	// The normal Go test build directory is private to its owner. Copy only
	// this fixture into an accessible temporary directory for the other user.
	directory := t.TempDir()
	for _, path := range []string{filepath.Dir(directory), directory} {
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(directory, "worker")
	target, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(target, source)
	target.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []Identity{{User: account.Username, Group: group.Name}, {User: account.Uid, Group: group.Gid}} {
		for _, network := range []string{"tcp", "unix"} {
			reply := runIdentityWorker(t, path, identity, network)
			if reply["uid"] != account.Uid || reply["gid"] != group.Gid {
				t.Fatalf("worker identity: %v", reply)
			}
			for _, group := range strings.Split(reply["groups"], ",") {
				if group == "0" {
					t.Fatal("worker retained the supervisor's root group")
				}
			}
		}
	}
}

func TestSocketIdentityLinux(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := OpenListener("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		uid, gid = 65534, 1
	}
	identity := Identity{User: strconv.Itoa(uid), Group: strconv.Itoa(gid)}
	finish, err := identity.socket(path, 0600)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := after.Sys().(*syscall.Stat_t)
	if stat.Uid != uint32(uid) || stat.Gid != uint32(gid) || after.Mode().Perm() != 0600 {
		t.Errorf("unexpected socket ownership: %+v %v", stat, after.Mode())
	}
	if err := finish(false); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, current := before.Sys().(*syscall.Stat_t), restored.Sys().(*syscall.Stat_t)
	if previous.Uid != current.Uid || previous.Gid != current.Gid || before.Mode() != restored.Mode() {
		t.Fatal("socket permission rollback did not restore original metadata")
	}
}

func TestSocketModeReload(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	address := filepath.Join(directory, "web.sock")
	write(t, path, "watch: ['app/ooth.yaml']\n")
	poller, err := NewPoller()
	if err != nil {
		t.Fatal(err)
	}
	defer poller.Close()
	manager := &manager{poller: poller, services: make(map[string]*service), listeners: make(map[int32]*service), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	defer manager.shutdown()
	var original *serviceListener
	for _, setting := range []struct {
		text string
		bits os.FileMode
	}{{"", 0660}, {"0600", 0600}, {"0000", 0}, {"0666", 0666}, {"'u=rw,g=r,o='", 0640}, {"'a+rw'", 0666}} {
		mode := setting.text
		app := "name: web\ncommand: [worker]\nlisten: {network: unix, address: '" + address + "'}\n"
		if mode != "" {
			app += "socket_mode: " + mode + "\n"
		}
		write(t, filepath.Join(directory, "app", "ooth.yaml"), app)
		snapshot, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.apply(snapshot); err != nil {
			t.Fatal(err)
		}
		current := manager.services["web"].listener
		if original != nil && current != original {
			t.Fatal("mode update replaced the listening socket")
		}
		original = current
		info, err := os.Lstat(address)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != setting.bits {
			t.Fatalf("socket_mode %s produced %v", mode, info.Mode())
		}
	}
	write(t, filepath.Join(directory, "app", "ooth.yaml"), "command: [worker]\nlisten: {network: unix, address: '"+address+"'}\nsocket_mode: 01777\n")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted non-permission mode bits")
	}
}

func TestIdentityChangeDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged caller")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	release, err := (Identity{User: "root"}).apply(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner, err := newProcessOwner(filepath.Join(t.TempDir(), "missing"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if job, err := owner.Start(cmd); err == nil {
		job.Kill()
		owner.Wait(job)
		job.Close()
		t.Fatal("silently started despite denied identity change")
	}
}
