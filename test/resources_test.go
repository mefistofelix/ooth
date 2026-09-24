package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// Lifecycle/load fixtures test worker policy independently of the test host's
// instantaneous load. Resource gating has separate deterministic coverage.
const unlimitedTestResources = "resources: {max_cpu_percent: 0, min_available_memory_percent: 0}\n"

func TestResourceLimits(t *testing.T) {
	now := time.Now()
	limits := ResourceLimits{90, 10}
	for _, tc := range []struct {
		name    string
		sample  resourceSample
		blocked bool
	}{
		{"healthy", resourceSample{at: now, cpu: 89, availableMemory: 10}, false},
		{"cpu", resourceSample{at: now, cpu: 90, availableMemory: 50}, true},
		{"memory", resourceSample{at: now, cpu: 1, availableMemory: 9}, true},
		{"initial", resourceSample{}, true},
		{"stale", resourceSample{at: now.Add(-4 * time.Second)}, true},
		{"error", resourceSample{at: now, err: errors.New("measurement failed")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := limits.blocked(tc.sample, now); (got != "") != tc.blocked {
				t.Fatalf("blocked=%q", got)
			}
			if got := (ResourceLimits{}).blocked(tc.sample, now); got != "" {
				t.Fatal(got)
			}
		})
	}
}

func TestScalingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ooth.yaml")
	app := filepath.Join(dir, "app.yaml")
	write(t, path, "watch: [app.yaml]\n")
	write(t, app, "command: [worker]\n")
	snapshot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range snapshot.Apps {
		if app.ScaleAt != 80 || app.ScaleWindow != time.Second {
			t.Fatalf("scaling defaults: %+v", app)
		}
	}
	if snapshot.Resources != (ResourceLimits{90, 10}) {
		t.Fatal(snapshot.Resources)
	}
	write(t, path, "watch: [app.yaml]\nresources: {max_cpu_percent: '95%', min_available_memory_percent: 5}\n")
	write(t, app, "command: [worker]\nscale_at: 75%\nscale_window: 2s\n")
	snapshot, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Resources != (ResourceLimits{95, 5}) {
		t.Fatal(snapshot.Resources)
	}
	for _, invalid := range []string{"scale_at: 0", "scale_at: 101", "scale_at: .nan", "scale_window: -1s", "scale_delay: -1s", "scale_window: 1s\nscale_delay: 1s"} {
		write(t, app, "command: [worker]\n"+invalid+"\n")
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	write(t, app, "command: [worker]\nscale_delay: 50ms\n")
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	write(t, path, "watch: [app.yaml]\nresources: {max_cpu_percent: 101}\n")
	if _, err := Load(path); err == nil {
		t.Fatal("invalid global CPU limit")
	}
}

func TestResourceSampler(t *testing.T) {
	owner, err := newProcessOwner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithCancel(context.Background())
	samples := make(chan resourceSample)
	done := make(chan struct{})
	go func() { defer close(done); sampleResources(ctx, owner, samples) }()
	defer func() { cancel(); <-done }()
	select {
	case sample := <-samples:
		if sample.err != nil {
			t.Fatal(sample.err)
		}
		if sample.at.IsZero() || math.IsNaN(sample.cpu) || sample.cpu < 0 || sample.availableMemory <= 0 || sample.availableMemory > 100 {
			t.Fatalf("invalid sample: %+v", sample)
		}
		t.Logf("CPU %.2f%%, available RAM %.2f%%", sample.cpu, sample.availableMemory)
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not publish")
	}
}

func TestOccupancyWindow(t *testing.T) {
	now := time.Now()
	child := &process{ready: true, telemetry: true, active: make(map[string]time.Time)}
	service := &service{config: App{Concurrency: 1}, workers: map[*process]struct{}{child: {}}}
	service.observePressure(now)
	// Eight short requests, separated by gaps: no sustained saturation, but
	// 80% time-weighted occupancy. Account for events between scheduler ticks.
	for i := range 8 {
		started := now.Add(time.Duration(i) * 125 * time.Millisecond)
		child.active["request"] = started
		service.observePressure(started)
		delete(child.active, "request")
		service.observePressure(started.Add(100 * time.Millisecond))
	}
	service.observePressure(now.Add(time.Second))
	if !service.pressure.evaluate(now.Add(time.Second), time.Second, 79.9) {
		t.Fatal("short requests were lost")
	}
	if service.pressure.evaluate(now.Add(time.Second), time.Second, 1) {
		t.Fatal("same window grew twice")
	}
	child.active["request"] = now
	service.observePressure(now.Add(time.Second))
	second := &process{ready: true, telemetry: true, active: make(map[string]time.Time)}
	service.workers[second] = struct{}{}
	service.observePressure(now.Add(1500 * time.Millisecond))
	if service.pressure.evaluate(now.Add(2*time.Second), time.Second, 1) {
		t.Fatal("capacity change did not reset window")
	}
	service.observePressure(now.Add(2500 * time.Millisecond))
	if service.pressure.evaluate(now.Add(2500*time.Millisecond), time.Second, 80) {
		t.Fatal("one busy of two workers is only 50%")
	}
	second.telemetry = false
	service.observePressure(now.Add(3 * time.Second))
	if !service.pressure.since.IsZero() {
		t.Fatal("ordinary stdout worker allowed scaling")
	}
}

func TestResourceGrowthGate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner, err := newProcessOwner("", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	for _, minimum := range []int{0, 2} {
		now := time.Now()
		app := App{Command: []string{filepath.Join(t.TempDir(), "missing-worker")}, MinWorkers: minimum, MaxWorkers: 2, Concurrency: 1, ScaleAt: 80, ScaleWindow: time.Second, IdleTimeout: time.Hour}
		child := &process{ready: true, telemetry: true, started: now, config: app, active: map[string]time.Time{"request": now}}
		pool := &service{config: app, workers: map[*process]struct{}{child: {}}}
		child.service = pool
		manager := &manager{owner: owner, log: logger, services: map[string]*service{"web": pool}, workers: map[*process]struct{}{child: {}}, resourceLimits: ResourceLimits{90, 10}, resources: resourceSample{at: now, cpu: 99, availableMemory: 50}}
		manager.tick(now, false)
		manager.tick(now.Add(time.Second), false)
		if minimum == 2 {
			if pool.failures == 0 {
				t.Fatal("resource guard blocked configured minimum")
			}
			continue
		}
		if pool.failures != 0 {
			t.Fatal("attempted extra spawn under CPU pressure")
		}
		manager.resources = resourceSample{at: now.Add(2 * time.Second), cpu: 1, availableMemory: 50}
		manager.tick(now.Add(2*time.Second), false)
		if pool.failures != 1 {
			t.Fatal("did not attempt extra spawn after resources recovered")
		}
	}
}
