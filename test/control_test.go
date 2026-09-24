package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testWorkerListener() (net.Listener, error) {
	file := os.Stdin
	if value := os.Getenv("OOTH_LISTEN_HANDLE"); value != "" {
		handle, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, err
		}
		file = os.NewFile(uintptr(handle), "listener")
	}
	defer file.Close()
	return net.FileListener(file)
}

func testWorkerControl(stop context.CancelFunc) {
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "v=1 event=stop ts=") {
				stop()
				return
			}
		}
	}()
}

// Distinguish receipt of the protocol command from receipt of an OS signal.
func TestControlWorkerHelper(t *testing.T) {
	marker := os.Getenv("TEST_CONTROL_MARKER")
	if marker == "" {
		return
	}
	mode := os.Getenv("TEST_CONTROL_MODE")
	if os.Getenv("OOTH_LISTEN_HANDLE") != "" || mode == "legacy" {
		if mode == "numeric" && os.Getenv("OOTH_LISTEN_HANDLE") != "7" {
			os.Exit(3)
		}
		listener, err := testWorkerListener()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer listener.Close()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, StopSignal)
	commands := make(chan string, 1)
	if mode != "legacy" {
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			if scanner.Scan() {
				commands <- scanner.Text()
			}
		}()
	}
	if mode == "plain" {
		fmt.Fprintln(os.Stdout, "ordinary output")
	}
	WriteEvent(os.Stdout, Event{Type: "ready", Time: time.Now().UnixNano()})
	for {
		select {
		case line := <-commands:
			os.WriteFile(marker, []byte(line), 0600)
			if mode != "ignore" {
				os.Exit(0)
			}
		case <-signals:
			os.WriteFile(marker, []byte("signal"), 0600)
			os.Exit(0)
		}
	}
}

func TestWorkerStopTransport(t *testing.T) {
	for _, mode := range []string{"handle", "init", "plain", "legacy", "ignore", "numeric"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "numeric" && runtime.GOOS != "linux" {
				t.Skip("explicit fd is Linux-only")
			}
			directory := t.TempDir()
			marker := filepath.Join(directory, "stop")
			app := App{Name: "worker", Command: []string{os.Args[0], "-test.run=^TestControlWorkerHelper$"},
				Env:     map[string]string{"TEST_CONTROL_MARKER": marker, "TEST_CONTROL_MODE": mode},
				Startup: true, Ready: "started", MaxWorkers: 1, Concurrency: 1,
				IdleTimeout: time.Minute, StartTimeout: time.Second, ScaleWindow: time.Second, StopTimeout: 300 * time.Millisecond}
			if mode != "init" {
				app.Listen = Socket{"tcp", freeAddress(t)}
			}
			if mode == "legacy" {
				app.SocketHandoff = "stdin"
			}
			if mode == "numeric" {
				app.SocketHandoff = "7"
			}
			path := filepath.Join(directory, "ooth.yaml")
			write(t, path, "watch: ['worker.yaml']\n")
			writeApp(t, filepath.Join(directory, "worker.yaml"), app)
			log := &proxyLog{apps: make(map[string]proxyWorkerState)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, path, slog.New(log)) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			for {
				log.mu.Lock()
				detected := strings.Contains(strings.Join(log.lines, "\n"), "worker stdout detected")
				log.mu.Unlock()
				if detected {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("worker did not report stdout mode")
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			deadline := time.Now().Add(3 * time.Second)
			for log.state("worker").live != 0 {
				if time.Now().After(deadline) {
					t.Fatal("worker did not stop")
				}
				time.Sleep(10 * time.Millisecond)
			}
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "plain" || mode == "legacy" {
				if string(data) != "signal" {
					t.Fatalf("expected signal, received %q", data)
				}
			} else if !strings.HasPrefix(string(data), "v=1 event=stop ts=") {
				t.Fatalf("expected stdin command, received %q", data)
			}
			if mode == "ignore" {
				log.mu.Lock()
				lines := strings.Join(log.lines, "\n")
				log.mu.Unlock()
				if !strings.Contains(lines, "worker exceeded graceful timeout") {
					t.Fatalf("missing forced termination: %s", lines)
				}
			}
		})
	}
}

func TestSocketHandoffConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ooth.yaml")
	write(t, path, "watch: ['worker.yaml']\n")
	for _, value := range []string{"env", "stdin", "0", "3", "7", "1", "2", "-1", "1025", "unknown"} {
		write(t, filepath.Join(filepath.Dir(path), "worker.yaml"), "command: [worker]\nlisten: {network: tcp, address: '127.0.0.1:8080'}\nsocket_handoff: "+value+"\n")
		_, err := Load(path)
		valid := value == "env" || value == "stdin" || value == "0" || (runtime.GOOS == "linux" && (value == "3" || value == "7"))
		if (err == nil) != valid {
			t.Errorf("socket_handoff=%s error=%v", value, err)
		}
	}
}
