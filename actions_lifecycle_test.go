package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDependencyReadinessLossAndRecovery(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	database := actionApp(t, "database")
	database.Actions = map[string]Action{"probe": {HTTP: &HTTPRequest{URL: server.URL}}}
	database.Triggers = map[string][]Trigger{"health": {{Action: "probe", Scope: "app", Interval: 20 * time.Millisecond, FailureThreshold: 2, OnFailure: "log"}}}
	started, waited, parallel := actionApp(t, "started"), actionApp(t, "waited"), actionApp(t, "parallel")
	started.Dependencies = map[string]string{"database": "started"}
	waited.Dependencies = map[string]string{"database": "ready"}
	parallel.Dependencies = map[string]string{"database": "parallel"}
	manager := actionManager(t, database, started, waited, parallel)
	pumpActions(t, manager, false, func() bool {
		for _, service := range manager.services {
			if readyWorkers(service) != 1 {
				return false
			}
		}
		return true
	})
	initial := make(map[string]*process)
	for child := range manager.workers {
		initial[child.config.Name] = child
	}
	healthy.Store(false)
	pumpActions(t, manager, false, func() bool {
		return !initial["waited"].stopping.IsZero() && !initial["parallel"].ready
	})
	for _, name := range []string{"database", "started", "parallel"} {
		if !initial[name].stopping.IsZero() || initial[name].exited {
			t.Fatalf("readiness loss unexpectedly retired %s", name)
		}
	}
	if !initial["started"].ready {
		t.Fatal("started dependency incorrectly gated on readiness")
	}
	healthy.Store(true)
	pumpActions(t, manager, false, func() bool {
		return readyWorkers(manager.services["waited"]) == 1 && initial["parallel"].ready
	})
	for child := range manager.services["waited"].workers {
		if child.ready && child == initial["waited"] {
			t.Fatal("retired dependent was reused")
		}
	}
}

func TestDependencyShutdownWaitsForPostStop(t *testing.T) {
	postStopStarted := make(chan struct{}, 1)
	postStopContext, releasePostStop := context.WithCancel(context.Background())
	var databaseStop atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/front":
			postStopStarted <- struct{}{}
			select {
			case <-postStopContext.Done():
			case <-request.Context().Done():
			}
		case "/database":
			databaseStop.Store(true)
		}
	}))
	defer server.Close()
	defer releasePostStop()
	database, front := actionApp(t, "database"), actionApp(t, "front")
	database.Actions = map[string]Action{"notify": {HTTP: &HTTPRequest{URL: server.URL + "/database"}}}
	database.Triggers = map[string][]Trigger{"pre_stop": {{Action: "notify"}}}
	front.Requires = []string{"database"}
	front.Actions = map[string]Action{"cleanup": {HTTP: &HTTPRequest{URL: server.URL + "/front"}}}
	front.Triggers = map[string][]Trigger{"post_stop": {{Action: "cleanup", Scope: "app"}}}
	manager := actionManager(t, database, front)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services["front"]) == 1 })
	pumpActions(t, manager, true, func() bool { return len(postStopStarted) != 0 })
	if databaseStop.Load() || readyWorkers(manager.services["database"]) != 1 {
		t.Fatal("prerequisite stopped before dependent post_stop completed")
	}
	releasePostStop()
	pumpActions(t, manager, true, func() bool { return len(manager.workers) == 0 && manager.actionTasks == 0 })
	if !databaseStop.Load() {
		t.Fatal("prerequisite did not receive its stop hook")
	}
}

func TestPeriodicActionRuntimeSnapshotAndNoOverlap(t *testing.T) {
	var active, completed atomic.Int32
	var overlap, readySeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if active.Add(1) > 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		if request.Header.Get("X-Ready") == "true" && request.Header.Get("X-Ready-Workers") == "1" {
			readySeen.Store(true)
		}
		select {
		case <-time.After(35 * time.Millisecond):
			fmt.Fprint(writer, "healthy")
			completed.Add(1)
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	app := actionApp(t, "snapshots")
	app.Actions = map[string]Action{"probe": {HTTP: &HTTPRequest{URL: server.URL, Headers: map[string]string{
		"X-Ready": "{{.runtime.ready}}", "X-Ready-Workers": "{{.runtime.ready_workers}}",
	}}, Expect: Expectation{Contains: "healthy"}}}
	app.Triggers = map[string][]Trigger{"health": {{Action: "probe", Interval: 5 * time.Millisecond}}}
	manager := actionManager(t, app)
	pumpActions(t, manager, false, func() bool { return completed.Load() >= 5 })
	if overlap.Load() {
		t.Fatal("periodic invocations overlapped")
	}
	if !readySeen.Load() {
		t.Fatal("periodic actions never observed the already-ready worker")
	}
}
