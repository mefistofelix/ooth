package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNodeWorker(t *testing.T) {
	node := os.Getenv("OOTH_TEST_NODE")
	if node == "" {
		t.Skip("set OOTH_TEST_NODE to test the experimental Node worker; Windows needs Node 26.1+ with FFI")
	}
	directory := t.TempDir()
	worker, err := filepath.Abs("tools/probes/node_worker.cjs")
	if err != nil {
		t.Fatal(err)
	}
	stopFile := filepath.Join(directory, "stopped")
	address := freeAddress(t)
	app := App{Name: "node", Command: []string{node, worker}, Env: map[string]string{"TEST_NODE_STOPFILE": stopFile}, Listen: Socket{"tcp", address}, MaxWorkers: 1, Concurrency: 1, IdleTimeout: 250 * time.Millisecond, StartTimeout: 3 * time.Second, StopTimeout: 2 * time.Second, ScaleDelay: 50 * time.Millisecond}
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['app/ooth.yaml']\n")
	writeApp(t, filepath.Join(directory, "app", "ooth.yaml"), app)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Node supervisor shutdown timeout")
		}
	})
	waitStopped := func(pid string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for {
			data, err := os.ReadFile(stopFile)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if slices.Contains(strings.Fields(string(data)), pid) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("Node worker %s did not record graceful shutdown", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var previous string
	for _, delay := range []int{0, 300} {
		connection := connect(t, "tcp", address)
		fmt.Fprintf(connection, "GET /%d HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", delay)
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			connection.Close()
			t.Fatal(err)
		}
		if delay != 0 {
			// Headers confirm the request started; stop while its body is pending.
			cancel()
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		connection.Close()
		if err != nil || response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "worker=") {
			t.Fatalf("Node reply: %q %v", body, err)
		}
		pid := strings.TrimPrefix(strings.Fields(string(body))[0], "worker=")
		if pid == previous {
			t.Fatal("idle Node worker was not replaced")
		}
		waitStopped(pid)
		previous = pid
	}
}
