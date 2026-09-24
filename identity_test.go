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
	"reflect"
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
		"command: [worker]\nuser: alice\npassword: '" + password + "'\nuser: duplicate\n",
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

func TestMachinePasswordSyspermCompatibility(t *testing.T) {
	// Public fixture: sysperm's label (with NUL) followed by bytes 00 through 1f.
	secret := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	const expected = "Sp!9e30ca5f9c10d6dd0ce2169670a5c24d8aA0!"
	for _, encoded := range []string{secret, strings.ToUpper(secret)} {
		password, err := machinePassword(encoded)
		if err != nil || password != expected {
			t.Fatal("password differs from sysperm's byte format")
		}
	}
	changed, err := machinePassword(strings.Repeat("ab", 32))
	if err != nil || changed == expected {
		t.Fatal("changing the secret must change the password")
	}
}

func TestIdentityDefaultPasswordConfig(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	secret := strings.Repeat("ab", 32)
	root := "watch: ['apps/*.yaml']\nwindows_password_secret: '" + secret + "'\n"
	write(t, path, root)
	write(t, filepath.Join(directory, "apps", "alice.yaml"), "name: alice\ncommand: [worker]\nuser: alice\n")
	write(t, filepath.Join(directory, "apps", "bob.yaml"), "name: bob\ncommand: [worker]\nuser: bob\n")
	write(t, filepath.Join(directory, "apps", "current.yaml"), "name: current\ncommand: [worker]\n")
	if runtime.GOOS == "windows" {
		write(t, filepath.Join(directory, "apps", "explicit.yaml"), "name: explicit\ncommand: [worker]\nuser: alice\npassword: override\n")
		write(t, filepath.Join(directory, "apps", "empty.yaml"), "name: empty\ncommand: [worker]\nuser: alice\npassword: ''\n")
	}
	first, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := machinePassword(secret)
	for _, name := range []string{"alice", "bob"} {
		app := first.Apps[name]
		if runtime.GOOS == "windows" {
			if app.Identity.Password == nil || *app.Identity.Password != expected {
				t.Fatal("missing derived password")
			}
		} else if app.Identity.Password != nil {
			t.Fatal("Windows default affected Linux credentials")
		}
		if _, exists := app.Values["password"]; exists {
			t.Fatal("derived password leaked into YAML placeholder values")
		}
		command, environment, err := app.expandLaunch("")
		if err != nil {
			t.Fatal(err)
		}
		values := append([]string(nil), command...)
		for _, value := range environment {
			values = append(values, value)
		}
		for _, value := range values {
			if strings.Contains(value, expected) || strings.Contains(value, secret) {
				t.Fatal("credentials leaked into process arguments or environment")
			}
		}
	}
	if first.Apps["current"].Identity.Password != nil {
		t.Fatal("default password requires an explicit user")
	}
	if runtime.GOOS == "windows" {
		for name, expected := range map[string]string{"explicit": "override", "empty": ""} {
			password := first.Apps[name].Identity.Password
			if password == nil || *password != expected {
				t.Fatal("explicit password must take precedence, including empty")
			}
		}
	}
	write(t, path, strings.Replace(root, secret, strings.Repeat("cd", 32), 1))
	second, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, app := range first.Apps {
		changed := !reflect.DeepEqual(app, second.Apps[name])
		if changed != (runtime.GOOS == "windows" && (name == "alice" || name == "bob")) {
			t.Fatalf("secret rotation changed the wrong application: %s", name)
		}
	}
	write(t, path, "watch: ['apps/*.yaml']\n")
	withoutSecret, err := Load(path)
	if err != nil || withoutSecret.Apps["alice"].Identity.Password != nil {
		t.Fatal("omitted secret must preserve existing password-free behavior")
	}
}

func TestIdentitySecretErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ooth.yaml")
	for _, secret := range []string{"", "not-a-hex-secret", strings.Repeat("a", 63), strings.Repeat("a", 66)} {
		write(t, path, "watch: ['apps/*.yaml']\nwindows_password_secret: '"+secret+"'\n")
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), "windows_password_secret") {
			t.Fatal("invalid secret must reject configuration with a field-specific error")
		}
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatal("invalid secret disclosed in diagnostics")
		}
	}
	secret := strings.Repeat("ab", 32)
	for _, field := range []string{"['" + secret + "']", "'" + secret} {
		write(t, path, "watch: ['apps/*.yaml']\nwindows_password_secret: "+field+"\n")
		_, err := Load(path)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("malformed secret YAML must fail without disclosing its contents")
		}
	}
}
