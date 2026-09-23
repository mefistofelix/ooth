package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Compatibility audit, including a documented negative Windows result. This
// test is not an end-to-end activated HTTP worker certification.
func TestTrueAsyncCompatibility(t *testing.T) {
	php := os.Getenv("OOTH_TEST_TRUEASYNC")
	if php == "" {
		t.Skip("set OOTH_TEST_TRUEASYNC to the portable TrueAsync 0.10.0 PHP executable")
	}
	php, err := filepath.Abs(php)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-n", "-d", "display_errors=stderr"}
	if runtime.GOOS == "windows" {
		args = append(args, "-d", "extension_dir="+filepath.Join(filepath.Dir(php), "ext"), "-d", "extension=php_sockets.dll", "-d", "extension=php_true_async_server.dll")
	}
	command := func(ctx context.Context, script string, extra ...string) *exec.Cmd {
		params := append(append([]string{}, args...), filepath.Join("tools", "probes", "trueasync", script))
		cmd := exec.CommandContext(ctx, php, append(params, extra...)...)
		cmd.NewProcessGroup = true
		return cmd
	}
	t.Run("inherited_socket", func(t *testing.T) {
		listener, err := OpenListener("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := command(ctx, "socket.php")
		cmd.Stdin = listener.File
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		}()
		reader := bufio.NewReader(stdout)
		line, readErr := reader.ReadString('\n')
		if runtime.GOOS == "windows" {
			waitErr := cmd.Wait()
			if waitErr == nil || readErr == nil || !strings.Contains(diagnostic.String(), "Failed to open TCP handle") || strings.Contains(diagnostic.String(), "script started") {
				t.Fatalf("expected known pre-script failure, got stdout=%q stderr=%q wait=%v", line, diagnostic.String(), waitErr)
			}
			t.Log("confirmed runtime blocker before PHP script: " + diagnostic.String())
			return
		}
		if readErr != nil || line != "ready\n" {
			t.Fatalf("ready=%q error=%v", line, readErr)
		}
		for range 3 {
			connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			connection.SetDeadline(time.Now().Add(time.Second))
			reply, err := bufio.NewReader(connection).ReadString('\n')
			connection.Close()
			if err != nil || reply != fmt.Sprintln(cmd.Process.Pid) {
				t.Fatalf("reply=%q error=%v", reply, err)
			}
		}
		t.Log("three accepts on inherited stdin duplicate, no bind or parent proxy")
	})
	t.Run("native_http1_and_h2c", func(t *testing.T) {
		address := freeAddress(t)
		_, port, _ := net.SplitHostPort(address)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := command(ctx, "server.php", port)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			cmd.Process.Kill()
			cmd.Wait()
			if t.Failed() {
				t.Log(output.String())
			}
		}()
		connection := connect(t, "tcp", address)
		connection.Close()
		for _, h2 := range []bool{false, true} {
			protocols := new(http.Protocols)
			protocols.SetHTTP1(!h2)
			protocols.SetUnencryptedHTTP2(h2)
			transport := &http.Transport{Protocols: protocols}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			response, err := client.Get("http://" + address + "/")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || string(body) != "trueasync native\n" || (response.ProtoMajor == 2) != h2 {
				t.Fatalf("protocol=%s status=%d body=%q error=%v", response.Proto, response.StatusCode, body, err)
			}
			t.Log(response.Proto + " response from built-in server (control listener)")
		}
	})
}
