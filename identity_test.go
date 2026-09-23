package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIdentityHelper(t *testing.T) {
	if os.Getenv("TEST_IDENTITY_WORKER") != "1" {
		return
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, groups := account.Uid, account.Gid, "-"
	if runtime.GOOS == "linux" {
		uid, gid = strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid())
		memberships, err := os.Getgroups()
		if err != nil {
			t.Fatal(err)
		}
		var values []string
		for _, value := range memberships {
			values = append(values, strconv.Itoa(value))
		}
		groups = strings.Join(values, ",")
	}
	listener, err := net.FileListener(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(connection, "uid=%s gid=%s groups=%s\n", uid, gid, groups)
	connection.Close()
	listener.Close()
	os.Exit(0)
}

func runIdentityWorker(t *testing.T, executable string, identity Identity, network string) map[string]string {
	t.Helper()
	owner, err := newProcessOwner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	address := "127.0.0.1:0"
	if network == "unix" {
		address = filepath.Join(t.TempDir(), "worker.sock")
	}
	listener, err := OpenListener(network, address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if network == "unix" {
		finish, err := identity.socket(address, 0660)
		if err != nil {
			t.Fatal(err)
		}
		if err := finish(true); err != nil {
			t.Fatal(err)
		}
	}
	file, err := listener.ChildFile()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command(executable, "-test.run=^TestIdentityHelper$")
	cmd.Dir = filepath.Dir(executable)
	cmd.Env = append(os.Environ(), "TEST_IDENTITY_WORKER=1")
	cmd.Stdin, cmd.Stderr = file, os.Stderr
	cmd.NewProcessGroup = true
	release, err := identity.apply(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	job, err := owner.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	waited := false
	defer func() {
		if !waited {
			job.Kill()
			owner.Wait(job)
		}
	}()
	connection, err := net.DialTimeout(network, listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(4 * time.Second))
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(job); err != nil {
		t.Fatal(err)
	}
	waited = true
	result := make(map[string]string)
	for _, field := range strings.Fields(line) {
		key, value, _ := strings.Cut(field, "=")
		result[key] = value
	}
	return result
}

func TestIdentityCurrentUser(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			reply := runIdentityWorker(t, executable, Identity{User: account.Username}, network)
			if reply["uid"] != account.Uid {
				t.Fatalf("worker identity: %v, expected %s", reply, account.Uid)
			}
		})
	}
}

func TestIdentityConfigAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	password := "ooth-example-secret-for-error-test"
	write(t, path, "command: [worker]\nuser: alice\npassword: '"+password+"'\n")
	var app App
	if err := read(path, &app); err != nil {
		t.Fatal(err)
	}
	if app.Identity.User != "alice" || app.Identity.Password == nil || *app.Identity.Password != password {
		t.Fatal("identity fields not decoded")
	}
	for _, invalid := range []string{
		"command: [worker]\nuser: alice\npassword: '" + password + "'\nunknown: 1\n",
		"command: [worker]\npassword: ['" + password + "']\n",
		"command: [worker]\npassword: '" + password + "\n",
	} {
		write(t, path, invalid)
		err := read(path, &app)
		if err == nil {
			t.Fatal("accepted malformed identity configuration")
		}
		if strings.Contains(err.Error(), password) {
			t.Fatal("YAML diagnostic disclosed password")
		}
	}
}
