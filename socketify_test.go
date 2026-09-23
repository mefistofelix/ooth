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
	"strings"
	"testing"
	"time"
)

func TestSocketifyExtraHandle(t *testing.T) {
	python := os.Getenv("OOTH_TEST_SOCKETIFY")
	if python == "" {
		t.Skip("set OOTH_TEST_SOCKETIFY and prepare PYTHONPATH as documented in tools/probes/socketify")
	}
	listener, err := OpenListener("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request := func() string {
		t.Helper()
		response, err := client.Get("http://" + listener.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 || response.ProtoMajor != 1 {
			t.Fatalf("status=%d protocol=%s body=%q error=%v", response.StatusCode, response.Proto, body, err)
		}
		return string(body)
	}
	var commands []*exec.Cmd
	for index := 0; index < 3; index++ {
		cmd := exec.CommandContext(ctx, python, filepath.Join("tools", "probes", "socketify", "worker.py"))
		cmd.NewProcessGroup = true
		cmd.Stdin = strings.NewReader("ordinary stdin\n")
		handle, release := extraTestHandle(t, cmd, listener.File)
		cmd.Env = append(os.Environ(), "OOTH_LISTEN_HANDLE="+handle)
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			release()
			t.Fatal(err)
		}
		err = cmd.Start()
		release()
		if err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		defer func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
			if t.Failed() {
				t.Log(diagnostic.String())
			}
		}()
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "ready" {
			t.Fatalf("ready=%q error=%v", line, err)
		}
		served := false
		for attempt := 0; attempt < 100 && !served; attempt++ {
			served = request() == fmt.Sprintln(cmd.Process.Pid)
		}
		if !served {
			t.Fatalf("new worker %d never served on the shared listener", cmd.Process.Pid)
		}
		t.Logf("HTTP/1.1 from worker=%d extra=%s, earlier workers still running", cmd.Process.Pid, handle)
	}
	// Exercise close callbacks as well as startup. The remaining processes must
	// continue accepting after the first one's listener is closed normally.
	for _, cmd := range commands {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if cmd != commands[len(commands)-1] {
			if body := request(); body == fmt.Sprintln(cmd.Process.Pid) {
				t.Fatalf("response from closed worker: %q", body)
			}
		}
	}
	t.Log("all three workers closed normally; surviving workers retained their listener")
}
