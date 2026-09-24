package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestJavaScriptExtraHandle(t *testing.T) {
	for _, name := range []string{"NODE", "BUN", "DENO", "DENO_DIRECT"} {
		t.Run(name, func(t *testing.T) {
			if name == "DENO_DIRECT" && runtime.GOOS != "windows" {
				t.Skip("Windows-only negative probe")
			}
			executable := os.Getenv("OOTH_TEST_" + strings.TrimSuffix(name, "_DIRECT"))
			if executable == "" {
				t.Skip("set OOTH_TEST_" + name)
			}
			listener, err := OpenListener("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			request := func(delay string) *http.Response {
				t.Helper()
				response, err := client.Get("http://" + listener.Addr().String() + "/" + delay)
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			body := func(response *http.Response) string {
				t.Helper()
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != 200 || response.ProtoMajor != 1 {
					t.Fatalf("status=%d protocol=%s body=%q error=%v", response.StatusCode, response.Proto, data, err)
				}
				return string(data)
			}
			type worker struct {
				cmd    *exec.Cmd
				input  io.WriteCloser
				output *bufio.Reader
			}
			var workers []worker
			for index := 0; index < 3; index++ {
				args := []string{filepath.Join("..", "test", "integration", "javascript", "worker.cjs")}
				if strings.HasPrefix(name, "DENO") {
					args = append([]string{"run", "-A"}, args...)
				}
				cmd := exec.CommandContext(ctx, executable, args...)
				file, err := listener.ChildFile()
				if err != nil {
					t.Fatal(err)
				}
				handle, err := inheritListener(cmd, file, 3)
				if err != nil {
					file.Close()
					t.Fatal(err)
				}
				cmd.Env = append(os.Environ(), fmt.Sprintf("OOTH_LISTEN_HANDLE=%d", handle))
				if name == "DENO_DIRECT" {
					cmd.Env = append(cmd.Env, "TEST_DENO_DIRECT=1")
				}
				input, err := cmd.StdinPipe()
				if err != nil {
					file.Close()
					t.Fatal(err)
				}
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					file.Close()
					t.Fatal(err)
				}
				var diagnostic bytes.Buffer
				cmd.Stderr = &diagnostic
				err = cmd.Start()
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if cmd.ProcessState == nil {
						cmd.Process.Kill()
						cmd.Wait()
					}
					input.Close()
					if t.Failed() {
						t.Log(diagnostic.String())
					}
				}()
				fmt.Fprintln(input, "ordinary stdin")
				output := bufio.NewReader(stdout)
				line, err := output.ReadString('\n')
				if name == "DENO_DIRECT" {
					// The private open path avoids the public fd path's CRT abort, but the
					// pinned runtime still cannot import this Windows listening socket.
					waitErr := cmd.Wait()
					if line != "" || err != io.EOF || waitErr == nil || !strings.Contains(diagnostic.String(), "uv_tcp_open: -4071") {
						t.Fatalf("unexpected Deno result ready=%q read=%v wait=%v stderr=%s", line, err, waitErr, diagnostic.String())
					}
					t.Log("known limitation reproduced: Windows listener import returns UV_EINVAL")
					return
				}
				if err != nil || strings.TrimSpace(line) != "ready" {
					t.Fatalf("ready=%q error=%v", line, err)
				}
				served := false
				for attempt := 0; attempt < 100 && !served; attempt++ {
					served = body(request("0")) == fmt.Sprintln(cmd.Process.Pid)
				}
				if !served {
					t.Fatalf("worker %d never served on shared listener", cmd.Process.Pid)
				}
				workers = append(workers, worker{cmd, input, output})
				t.Logf("HTTP/1.1 body from worker=%d handle=%d with earlier workers alive", cmd.Process.Pid, handle)
			}
			for index, worker := range workers {
				t.Logf("stop worker=%d index=%d", worker.cmd.Process.Pid, index)
				var pending *http.Response
				if index == len(workers)-1 {
					pending = request("250")
				}
				fmt.Fprintf(worker.input, "v=1 event=stop ts=%d\n", time.Now().UnixNano())
				if pending != nil && body(pending) != fmt.Sprintln(worker.cmd.Process.Pid) {
					t.Fatal("pending response lost")
				}
				line, err := worker.output.ReadString('\n')
				if err != nil || strings.TrimSpace(line) != "stopped" {
					t.Fatalf("stop=%q error=%v", line, err)
				}
				if err := worker.cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if index < len(workers)-1 && body(request("0")) == fmt.Sprintln(worker.cmd.Process.Pid) {
					t.Fatal("closed worker answered")
				}
			}
			t.Log("stdin stop, surviving listeners and final active-response drain passed")
		})
	}
}
