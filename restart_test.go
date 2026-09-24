package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
)

func TestRestartRules(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: [app.yaml]\n")
	appPath := filepath.Join(directory, "app.yaml")
	for _, invalid := range []string{
		"- glob: ''", "- glob: '[broken'", "- glob: '**/*.go'",
		"- glob: '*.go'\n  events: [not-an-event]",
	} {
		write(t, appPath, "command: [worker]\nrestart_on:\n"+invalid+"\n")
		if _, err := Load(path); err == nil {
			t.Errorf("accepted invalid restart rule: %s", invalid)
		}
	}
	write(t, appPath, "command: [worker]\ndirectory: elsewhere\nrestart_on:\n- glob: 'src/*/*.go'\n")
	snapshot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rule := snapshot.Apps[filepath.Base(directory)].RestartOn[0]
	if rule.Glob != filepath.Join(directory, "src", "*", "*.go") || !reflect.DeepEqual(rule.Events, []string{"create", "write", "remove", "rename"}) {
		t.Fatalf("wrong resolved glob/defaults: %+v", rule)
	}
	for _, test := range []struct {
		path string
		kind fswatcher.EventType
		want bool
	}{
		{"src/web/main.go", fswatcher.EventMod, true},
		{"src/web/main.go", fswatcher.EventCreate, true},
		{"src/web/main.go", fswatcher.EventRemove, true},
		{"src/web/main.go", fswatcher.EventRename, true},
		{"src/web/main.go", fswatcher.EventChmod, false},
		{"src/web/extra/main.go", fswatcher.EventMod, false},
		{"src/web/main.txt", fswatcher.EventMod, false},
		{"src/web", fswatcher.EventCreate, true},
		{"src/web", fswatcher.EventRename, true},
		{"src/web", fswatcher.EventMod, false},
		{"unrelated", fswatcher.EventRemove, false},
	} {
		event := fswatcher.WatchEvent{Path: filepath.Join(directory, filepath.FromSlash(test.path)), Types: []fswatcher.EventType{test.kind}}
		if got := rule.matches(event); got != test.want {
			t.Errorf("%s %s: match=%v, want %v", test.path, test.kind, got, test.want)
		}
	}
	rule.Events = []string{"chmod"}
	if !rule.matches(fswatcher.WatchEvent{Path: filepath.Join(directory, "src", "web", "main.go"), Types: []fswatcher.EventType{fswatcher.EventMod, fswatcher.EventChmod}}) {
		t.Fatal("event selection did not match an aggregated event")
	}
}

func waitRestart(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runRestartFixture(t *testing.T, app App) (*proxyLog, string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['*.app.yaml']\n"+unlimitedTestResources)
	appPath := filepath.Join(directory, "worker.app.yaml")
	writeApp(t, appPath, app)
	other := app
	other.Name, other.Listen, other.RestartOn = "unrelated", Socket{}, nil
	other.MinWorkers, other.MaxWorkers = 1, 1
	other.Env = map[string]string{"TEST_OOTH_WORKER": "1", "TEST_OOTH_VIRTUAL": "1"}
	writeApp(t, filepath.Join(directory, "other.app.yaml"), other)
	log := &proxyLog{apps: make(map[string]proxyWorkerState)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(log)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("restart fixture shutdown timed out")
		}
		if state := log.state("unrelated"); state.spawned != 1 || state.live != 0 {
			t.Errorf("restart leaked into unrelated app: %+v", state)
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		if app.Env["TEST_OOTH_IGNORE_STOP"] != "1" && strings.Contains(strings.Join(log.lines, "\n"), "worker exceeded graceful timeout") {
			t.Error("cooperative worker required forced termination")
		}
		if t.Failed() {
			t.Log(strings.Join(log.lines, "\n"))
		}
	})
	waitRestart(t, "initial workers not ready", func() bool {
		return log.state("unrelated").telemetry == 1 && log.state(app.Name).telemetry == app.MinWorkers
	})
	// Allow the separate Watch goroutine to register its initial roots.
	time.Sleep(150 * time.Millisecond)
	return log, appPath
}

func restartApp(source string) App {
	return App{Name: "worker", Command: []string{os.Args[0], "-test.run=^TestWorkerHelper$"},
		Env: map[string]string{"TEST_OOTH_WORKER": "1"}, Ready: "event",
		MinWorkers: 1, MaxWorkers: 1, Concurrency: 1, ScaleAt: 80, ScaleWindow: time.Minute,
		IdleTimeout: time.Minute, StartTimeout: 3 * time.Second, StopTimeout: 3 * time.Second,
		RestartOn: []RestartRule{{Glob: source}}}
}

func TestRestartOnFilesDrains(t *testing.T) {
	for _, kind := range []string{"init", "tcp", "unix", "tcp-zero", "own-listener"} {
		t.Run(kind, func(t *testing.T) {
			data := t.TempDir() // Outside the configuration watch root.
			source := filepath.Join(data, "code.txt")
			write(t, source, "old")
			app := restartApp(filepath.Join(data, "code.*"))
			app.Env["TEST_OOTH_SOURCE"] = source
			app.Env["TEST_OOTH_CLOSED"] = filepath.Join(data, "closed")
			if err := os.Mkdir(app.Env["TEST_OOTH_CLOSED"], 0700); err != nil {
				t.Fatal(err)
			}
			network, address := "tcp", freeAddress(t)
			switch kind {
			case "init":
				app.Env["TEST_OOTH_VIRTUAL"] = "1"
			case "own-listener":
				app.Env["TEST_OOTH_BIND"] = address
			case "tcp", "unix", "tcp-zero":
				if kind == "unix" {
					network, address = "unix", filepath.Join(data, "a.sock")
				}
				app.Listen = Socket{network, address}
				app.MinWorkers, app.MaxWorkers = 2, 2
				if kind == "tcp-zero" {
					app.MinWorkers, app.MaxWorkers = 0, 1
				}
			}
			log, _ := runRestartFixture(t, app)
			poolSize := max(app.MinWorkers, 1)
			var active []net.Conn
			if kind != "init" {
				for range poolSize {
					connection := connect(t, network, address)
					defer connection.Close()
					fmt.Fprintln(connection, "2s during-")
					active = append(active, connection)
				}
				waitRestart(t, "requests not active", func() bool { return log.state("worker").starts == poolSize })
			}
			// A save burst should produce one retirement per process.
			for _, value := range []string{"partial", "new"} {
				write(t, source, value)
				time.Sleep(20 * time.Millisecond)
			}
			if kind == "init" {
				waitRestart(t, "init process not restarted", func() bool { return log.state("worker").telemetry == 2 })
			} else {
				waitRestart(t, "old workers did not close accept before draining", func() bool {
					files, _ := filepath.Glob(filepath.Join(data, "closed", "*"))
					return len(files) == poolSize
				})
				if log.state("worker").ends != 0 {
					t.Fatal("stop did not reach workers during their active requests")
				}
				var queued net.Conn
				if app.Listen.Network != "" {
					// This must connect while every old worker has closed its listener:
					// the supervisor's original listening socket keeps the queue alive.
					var err error
					queued, err = net.DialTimeout(network, address, 200*time.Millisecond)
					if err != nil {
						t.Fatalf("parent listener was interrupted: %v", err)
					}
					defer queued.Close()
					queued.SetDeadline(time.Now().Add(5 * time.Second))
					fmt.Fprintln(queued, "1ms after-")
				}
				var reply string
				if queued != nil {
					var err error
					reply, err = bufio.NewReader(queued).ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					if state := log.state("worker"); state.live <= poolSize || state.ends > 1 {
						t.Fatalf("new request waited for old processes instead of overlapping drain: %+v", state)
					}
				} else {
					reply = request(t, network, address, "1ms after-")
				}
				if !strings.HasSuffix(reply, "after-new\n") {
					t.Fatalf("replacement did not reload source: %q", reply)
				}
				for _, connection := range active {
					line, err := bufio.NewReader(connection).ReadString('\n')
					if err != nil || !strings.HasSuffix(line, "during-old\n") {
						t.Fatalf("old request did not drain with old cached data: %q %v", line, err)
					}
				}
				waitRestart(t, "replacements not ready", func() bool { return log.state("worker").telemetry == 2*poolSize })
			}
			write(t, filepath.Join(data, "unrelated.tmp"), "ignored")
			time.Sleep(400 * time.Millisecond)
			if state := log.state("worker"); state.spawned != 2*poolSize || state.live != poolSize || state.starts != state.ends {
				t.Fatalf("extra restarts or incomplete drain: %+v", state)
			}
		})
	}
}

func TestRestartOnLazyApp(t *testing.T) {
	source := filepath.Join(t.TempDir(), "worker.txt")
	write(t, source, "old")
	app := restartApp(source)
	app.MinWorkers = 0
	app.Listen = Socket{"tcp", freeAddress(t)}
	app.Env["TEST_OOTH_SOURCE"] = source
	log, _ := runRestartFixture(t, app)
	write(t, source, "new")
	time.Sleep(350 * time.Millisecond)
	if log.state("worker").spawned != 0 {
		t.Fatal("file change woke a dormant app")
	}
	if reply := request(t, "tcp", app.Listen.Address, "1ms first-"); !strings.HasSuffix(reply, "first-new\n") {
		t.Fatal(reply)
	}
	write(t, source, "latest")
	waitRestart(t, "lazy worker not retired", func() bool { return log.state("worker").live == 0 })
	time.Sleep(200 * time.Millisecond)
	if log.state("worker").spawned != 1 {
		t.Fatal("retired lazy worker restarted without demand")
	}
	if reply := request(t, "tcp", app.Listen.Address, "1ms next-"); !strings.HasSuffix(reply, "next-latest\n") {
		t.Fatal(reply)
	}
}

func TestRestartOnEventsAndRuleReload(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "settings.txt")
	write(t, source, "initial")
	app := restartApp(filepath.Join(directory, "*.txt"))
	app.Env["TEST_OOTH_VIRTUAL"] = "1"
	app.RestartOn[0].Events = []string{"create", "rename", "remove"}
	log, appPath := runRestartFixture(t, app)
	expect := func(count int) {
		t.Helper()
		waitRestart(t, fmt.Sprintf("expected %d ready instances", count), func() bool { return log.state("worker").telemetry == count })
	}
	write(t, source, "write must be ignored")
	time.Sleep(350 * time.Millisecond)
	if log.state("worker").spawned != 1 {
		t.Fatal("unselected write event triggered a restart")
	}
	renamed := filepath.Join(directory, "renamed.txt")
	if err := os.Rename(source, renamed); err != nil {
		t.Fatal(err)
	}
	expect(2)
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	expect(3)
	write(t, source, "created")
	expect(4)
	temporary := filepath.Join(directory, "replacement.tmp")
	write(t, temporary, "atomic replacement")
	if err := os.Rename(temporary, source); err != nil {
		t.Fatal(err)
	}
	expect(5)

	// Changing the rule updates the watch roots and retires the old configuration.
	otherSource := filepath.Join(t.TempDir(), "other.txt")
	write(t, otherSource, "initial")
	app.RestartOn = []RestartRule{{Glob: otherSource, Events: []string{"write"}}}
	writeApp(t, appPath, app)
	expect(6)
	write(t, filepath.Join(directory, "old-rule.txt"), "ignored")
	time.Sleep(350 * time.Millisecond)
	if log.state("worker").spawned != 6 {
		t.Fatal("removed rule still triggered a restart")
	}
	write(t, otherSource, "new rule")
	expect(7)
	// A rejected YAML edit must not disable the last valid app's restart rules.
	write(t, appPath, "command: [unterminated\n")
	time.Sleep(300 * time.Millisecond)
	write(t, otherSource, "last valid rule")
	expect(8)
}

func TestRestartOnForceTimeout(t *testing.T) {
	source := filepath.Join(t.TempDir(), "settings.txt")
	write(t, source, "initial")
	app := restartApp(source)
	app.StopTimeout = 100 * time.Millisecond
	app.Env["TEST_OOTH_VIRTUAL"] = "1"
	app.Env["TEST_OOTH_IGNORE_STOP"] = "1"
	log, _ := runRestartFixture(t, app)
	write(t, source, "updated")
	waitRestart(t, "uncooperative worker not replaced and killed", func() bool {
		state := log.state("worker")
		return state.telemetry == 2 && state.live == 1
	})
	log.mu.Lock()
	defer log.mu.Unlock()
	if !strings.Contains(strings.Join(log.lines, "\n"), "worker exceeded graceful timeout") {
		t.Fatal("missing forced termination after graceful deadline")
	}
}

func TestFailedWorkerBackoffBeforeExit(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	service := &service{workers: make(map[*process]struct{})}
	child := &process{service: service, telemetry: true, control: writer,
		cmd: &exec.Cmd{Process: &os.Process{Pid: 1234}}}
	service.workers[child] = struct{}{}
	manager := &manager{workers: map[*process]struct{}{child: {}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Now()
	manager.handle(message{process: child, err: errors.New("bad telemetry")}, now)
	if child.stopping.IsZero() || !service.demand || !service.retryAt.After(now) || service.failures != 1 {
		t.Fatalf("failed worker became replaceable without backoff: %+v", service)
	}
	retryAt := service.retryAt
	manager.handle(message{process: child, err: errors.New("another bad line")}, now.Add(time.Millisecond))
	manager.handle(message{process: child, exited: true}, now.Add(2*time.Millisecond))
	if service.failures != 1 || !service.retryAt.Equal(retryAt) {
		t.Fatal("stopping/exiting the same failed worker applied backoff more than once")
	}
}
