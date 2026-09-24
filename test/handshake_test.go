package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlainWorkerStdout(t *testing.T) {
	for _, mode := range []string{"plain", "silent", "long", "partial"} {
		t.Run(mode, func(t *testing.T) {
			// Capture forwarded output, including the overlong first line and
			// later protocol-looking lines that must never enable telemetry.
			output, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = original; output.Close() }()
			address := freeAddress(t)
			directory := t.TempDir()
			path := filepath.Join(directory, "ooth.yaml")
			write(t, path, "watch: ['app/ooth.yaml']\n")
			app := App{Name: "web", Command: []string{os.Args[0], "-test.run=^TestWorkerHelper$"},
				Env:    map[string]string{"TEST_OOTH_WORKER": "1", "TEST_OOTH_STDOUT": mode},
				Listen: Socket{"tcp", address}, MaxWorkers: 2, Concurrency: 1,
				IdleTimeout: 100 * time.Millisecond, StartTimeout: time.Second,
				StopTimeout: time.Second, RequestTimeout: 100 * time.Millisecond, ScaleDelay: 25 * time.Millisecond}
			writeApp(t, filepath.Join(directory, "app", "ooth.yaml"), app)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("shutdown timeout")
				}
			}()
			first := strings.Fields(request(t, "tcp", address, "1ms first"))[0]
			time.Sleep(300 * time.Millisecond)
			if next := strings.Fields(request(t, "tcp", address, "1ms next"))[0]; next != first {
				t.Fatal("ordinary worker was stopped as idle")
			}
			long := connect(t, "tcp", address)
			defer long.Close()
			fmt.Fprintln(long, "400ms long")
			time.Sleep(150 * time.Millisecond)
			short := strings.Fields(request(t, "tcp", address, "1ms short"))[0]
			longReply, err := bufio.NewReader(long).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if short != first || strings.Fields(longReply)[0] != first {
				t.Fatal("ordinary worker was scaled or request-timeout killed")
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "long" && !strings.Contains(string(data), strings.Repeat("x", 9000)+"\n") {
				t.Fatal("long ordinary stdout line was truncated")
			}
			if mode == "plain" && !strings.Contains(string(data), "v=1 event=start") {
				t.Fatal("later protocol-looking output was not forwarded")
			}
		})
	}
}
