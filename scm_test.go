package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSCMConfig(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	appPath := filepath.Join(directory, "service.yaml")
	write(t, path, "watch: [service.yaml, other.yaml]\n")
	base := "name: database\nscm: {name: OothTestService, args: ['{{.vars.mode}}']}\nvars: {mode: maintenance}\nmin_workers: 1\n"
	write(t, appPath, base)
	snapshot, err := Load(path)
	if runtime.GOOS != "windows" {
		if err == nil {
			t.Fatal("SCM was accepted on a non-Windows platform")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	args, _, err := snapshot.Apps["database"].expandLaunch("")
	if err != nil || len(args) != 1 || args[0] != "maintenance" {
		t.Fatalf("SCM arguments: %v %v", args, err)
	}
	for _, extra := range []string{"command: [worker]\n", "env: {A: B}\n", "directory: .\n", "user: alice\n", "max_workers: 2\n", "ready: event\n", "listen: {network: tcp, address: ':12345'}\n"} {
		write(t, appPath, base+extra)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted conflicting SCM setting %q", extra)
		}
	}
	write(t, appPath, base)
	write(t, filepath.Join(directory, "other.yaml"), "name: other\nscm: {name: oothtestservice}\n")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted duplicate SCM ownership")
	}
}

type testSCMControl struct{ stops, kills int }

func (control *testSCMControl) Stop()       { control.stops++ }
func (control *testSCMControl) Kill() error { control.kills++; return nil }

func TestSCMRetirement(t *testing.T) {
	now := time.Now()
	app := restartApp("")
	app.SCM = &SCMService{Name: "test"}
	current := &service{name: "test", config: app, workers: make(map[*process]struct{})}
	control := &testSCMControl{}
	child := &process{scm: control, service: current, config: app, started: now}
	current.workers[child] = struct{}{}
	manager := &manager{services: map[string]*service{"test": current}, workers: map[*process]struct{}{child: {}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	manager.handle(message{process: child, scm: true, pid: 1234, ready: true}, now)
	if !child.ready || child.pid != 1234 || child.telemetry {
		t.Fatal("SCM running state did not set readiness without telemetry")
	}
	manager.restart(current, now)
	manager.tick(now.Add(time.Second), false)
	if control.stops != 1 || len(current.workers) != 1 || current.failures != 0 {
		t.Fatal("SCM restart attempted another instance before the old service stopped")
	}
	manager.tick(now.Add(app.StopTimeout), false)
	if control.kills != 1 {
		t.Fatal("SCM stop deadline did not force termination")
	}
	manager.handle(message{process: child, exited: true}, now.Add(app.StopTimeout))
	if len(current.workers) != 0 {
		t.Fatal("SCM stopped notification did not release the instance")
	}
}
