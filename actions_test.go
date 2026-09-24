package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestActionHelper(t *testing.T) {
	if os.Getenv("OOTH_ACTION_TEST") != "1" {
		return
	}
	delay, _ := time.ParseDuration(os.Getenv("ACTION_DELAY"))
	time.Sleep(delay)
	if marker := os.Getenv("ACTION_MARKER"); marker != "" {
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(90)
		}
		fmt.Fprintln(file, os.Getenv("ACTION_TEXT"))
		file.Close()
	}
	fmt.Print(os.Getenv("ACTION_OUTPUT"))
	code, _ := strconv.Atoi(os.Getenv("ACTION_EXIT"))
	os.Exit(code)
}

func actionCommand(t *testing.T) Action {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Action{Command: []string{executable, "-test.run=^TestActionHelper$"}, Env: map[string]string{"OOTH_ACTION_TEST": "1"}, Timeout: time.Second, Backoff: time.Millisecond}
}

func TestActionExecution(t *testing.T) {
	owner, err := newProcessOwner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	app := App{Name: "example", Command: []string{"unused"}, Directory: t.TempDir(), Vars: map[string]string{"answer": "healthy"}}
	values, err := app.templateValues("")
	if err != nil {
		t.Fatal(err)
	}
	command := actionCommand(t)
	command.Env["ACTION_OUTPUT"] = "{{.vars.answer}} {{.runtime.pid}}"
	command.Env["ACTION_EXIT"] = "7"
	command.Expect = Expectation{Contains: "healthy", Regexp: `healthy 0$`, ExitCodes: []int{7}}
	if err := command.execute(context.Background(), owner, app, values); err != nil {
		t.Fatal(err)
	}
	command.Expect.ExitCodes = nil
	if err := command.execute(context.Background(), owner, app, values); err == nil {
		t.Fatal("accepted exit code 7")
	}
	command.Env["ACTION_DELAY"] = "5s"
	command.Timeout = 40 * time.Millisecond
	started := time.Now()
	if err := command.execute(context.Background(), owner, app, values); err == nil || time.Since(started) > time.Second {
		t.Fatalf("unbounded command timeout: %v", err)
	}

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		if request.Method != "POST" || request.Header.Get("X-Check") != "healthy" || string(data) != "example" {
			writer.WriteHeader(400)
			return
		}
		if attempts.Add(1) < 3 {
			writer.WriteHeader(503)
			return
		}
		fmt.Fprint(writer, "healthy 42")
	}))
	defer server.Close()
	httpAction := Action{HTTP: &HTTPRequest{URL: server.URL, Method: "POST", Headers: map[string]string{"X-Check": "{{.vars.answer}}"}, Body: "{{.name}}"}, Expect: Expectation{Contains: "healthy", Regexp: `42$`}, Timeout: time.Second, Retries: 2, Backoff: time.Millisecond}
	if err := httpAction.execute(context.Background(), owner, app, values); err != nil || attempts.Load() != 3 {
		t.Fatalf("HTTP retry/match: %v, %d", err, attempts.Load())
	}
	if httpAction.HTTP.Headers["X-Check"] != "{{.vars.answer}}" {
		t.Fatal("template expansion mutated configuration")
	}

	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			var address string
			if network == "tcp" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.Addr().String()
				go func() {
					connection, err := listener.Accept()
					if err != nil {
						return
					}
					defer connection.Close()
					data := make([]byte, 4)
					io.ReadFull(connection, data)
					io.WriteString(connection, "po")
					time.Sleep(10 * time.Millisecond)
					io.WriteString(connection, "ng")
				}()
			} else {
				listener, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				address = listener.LocalAddr().String()
				go func() {
					data := make([]byte, 32)
					_, peer, err := listener.ReadFrom(data)
					if err == nil {
						listener.WriteTo([]byte("pong"), peer)
					}
				}()
			}
			action := Action{Expect: Expectation{Contains: "pong"}, Timeout: time.Second}
			check := &SocketCheck{Address: address, Send: "ping"}
			if network == "tcp" {
				action.TCP = check
			} else {
				action.UDP = check
			}
			if err := action.execute(context.Background(), owner, app, values); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActionConfiguration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	appPath := filepath.Join(directory, "app.yaml")
	write(t, path, "watch: [app.yaml]\n")
	valid := "name: app\ncommand: [worker, '{{.config.custom.label}}', '{{.runtime.pid}}']\ncustom: {label: hello}\nactions:\n  probe:\n    http: {url: 'http://127.0.0.1:1234/{{.config.custom.label}}', headers: {X-Pid: '{{.runtime.pid}}'}}\n    expect: {contains: hello}\ntriggers:\n  readiness: [{action: probe, scope: app, interval: 100ms, failure_threshold: 4}]\n"
	write(t, appPath, valid)
	snapshot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := snapshot.Apps["app"]
	args, _, err := app.expandLaunch("")
	if err != nil || args[1] != "hello" || args[2] != "0" {
		t.Fatalf("common config/runtime templates: %v %v", args, err)
	}
	for _, invalid := range []string{
		strings.Replace(valid, "action: probe", "action: missing", 1),
		strings.Replace(valid, "scope: app", "scope: invalid", 1),
		strings.Replace(valid, "failure_threshold: 4", "failure_threshold: -1", 1),
		strings.Replace(valid, "contains: hello", "regexp: '['", 1),
		strings.Replace(valid, "{{.runtime.pid}}", "{{.runtime.typo}}", 1),
		"command: [worker]\nactions: {probe: {udp: {address: '127.0.0.1:53'}}}\n",
		valid + "dependencies: {other: invalid}\n",
	} {
		write(t, appPath, invalid)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid config: %s", invalid)
		}
	}
}

func actionApp(t *testing.T, name string) App {
	executable, _ := os.Executable()
	return App{Name: name, Command: []string{executable, "-test.run=^TestWorkerHelper$"}, Env: map[string]string{"TEST_OOTH_WORKER": "1", "TEST_OOTH_VIRTUAL": "1"}, Ready: "event", MinWorkers: 1, MaxWorkers: 2, Concurrency: 1, IdleTimeout: time.Hour, StartTimeout: 4 * time.Second, StopTimeout: 400 * time.Millisecond, ScaleWindow: time.Second, ScaleAt: 80, Directory: t.TempDir()}
}

func actionManager(t *testing.T, apps ...App) *manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner, err := newProcessOwner("", logger)
	if err != nil {
		t.Fatal(err)
	}
	manager := &manager{owner: owner, services: make(map[string]*service), workers: make(map[*process]struct{}), messages: make(chan message, 256), done: make(chan struct{}), log: logger}
	for _, app := range apps {
		if err := app.validateActions(); err != nil {
			t.Fatal(err)
		}
		manager.services[app.Name] = &service{name: app.Name, config: app, workers: make(map[*process]struct{})}
	}
	t.Cleanup(func() {
		for child := range manager.workers {
			manager.stop(child, time.Now(), "test cleanup")
		}
		pumpActions(t, manager, true, func() bool { return len(manager.workers) == 0 && manager.actionTasks == 0 })
		owner.Close()
		close(manager.done)
	})
	return manager
}

func pumpActions(t *testing.T, manager *manager, stopping bool, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !done() && time.Now().Before(deadline) {
		select {
		case msg := <-manager.messages:
			manager.handle(msg, time.Now())
		case <-time.After(5 * time.Millisecond):
		}
		manager.tick(time.Now(), stopping)
	}
	if !done() {
		t.Fatalf("scheduler did not reach expected state: workers=%d actions=%d", len(manager.workers), manager.actionTasks)
	}
}

func readyWorkers(service *service) int {
	count := 0
	for child := range service.workers {
		if child.ready && child.stopping.IsZero() {
			count++
		}
	}
	return count
}

func TestActionScopesAndLifecycle(t *testing.T) {
	app := actionApp(t, "pool")
	app.MinWorkers = 2
	app.Actions = make(map[string]Action)
	app.Triggers = make(map[string][]Trigger)
	markerDir := t.TempDir()
	for _, event := range []string{"pre_start", "post_start", "pre_stop", "post_stop"} {
		for _, scope := range []string{"app", "worker"} {
			name := event + "-" + scope
			action := actionCommand(t)
			action.Env["ACTION_MARKER"] = filepath.Join(markerDir, name)
			action.Env["ACTION_TEXT"] = "{{.runtime.event}} {{.runtime.scope}} {{.runtime.pid}}"
			app.Actions[name] = action
			app.Triggers[event] = append(app.Triggers[event], Trigger{Action: name, Scope: scope})
		}
	}
	manager := actionManager(t, app)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 2 })
	for child := range manager.workers {
		manager.stop(child, time.Now(), "test lifecycle")
	}
	pumpActions(t, manager, true, func() bool { return len(manager.workers) == 0 && manager.actionTasks == 0 })
	for _, event := range []string{"pre_start", "post_start", "pre_stop", "post_stop"} {
		for _, scope := range []string{"app", "worker"} {
			data, err := os.ReadFile(filepath.Join(markerDir, event+"-"+scope))
			if err != nil {
				t.Fatal(err)
			}
			count := 1
			if scope == "worker" {
				count = 2
			}
			if strings.Count(string(data), "\n") != count || !strings.Contains(string(data), event+" "+scope) {
				t.Fatalf("%s %s hooks: %s", event, scope, data)
			}
		}
	}
}

func TestReadinessDependencyConditions(t *testing.T) {
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			writer.WriteHeader(503)
		}
	}))
	defer server.Close()
	db := actionApp(t, "db")
	db.Actions = map[string]Action{"ready": {HTTP: &HTTPRequest{URL: server.URL}}}
	db.Triggers = map[string][]Trigger{"readiness": {{Action: "ready", Scope: "app", Interval: 20 * time.Millisecond, OnFailure: "log", FailureThreshold: 100}}}
	started, waited, parallel := actionApp(t, "started"), actionApp(t, "waited"), actionApp(t, "parallel")
	started.Dependencies = map[string]string{"db": "started"}
	waited.Requires = []string{"db"}
	parallel.Dependencies = map[string]string{"db": "parallel"}
	manager := actionManager(t, db, started, waited, parallel)
	pumpActions(t, manager, false, func() bool {
		return readyWorkers(manager.services["started"]) == 1 && len(manager.services["parallel"].workers) == 1
	})
	if len(manager.services["waited"].workers) != 0 || readyWorkers(manager.services["parallel"]) != 0 || readyWorkers(manager.services["db"]) != 0 {
		t.Fatal("readiness gate bypassed")
	}
	healthy.Store(true)
	pumpActions(t, manager, false, func() bool {
		return readyWorkers(manager.services["waited"]) == 1 && readyWorkers(manager.services["parallel"]) == 1
	})
}

func TestPeriodicActionRecovery(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			writer.WriteHeader(503)
		}
	}))
	defer server.Close()
	app := actionApp(t, "health")
	app.Actions = map[string]Action{"health": {HTTP: &HTTPRequest{URL: server.URL}}}
	app.Triggers = map[string][]Trigger{"health": {{Action: "health", Interval: 20 * time.Millisecond, FailureThreshold: 3}}}
	manager := actionManager(t, app)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	var old *process
	for child := range manager.workers {
		old = child
	}
	healthy.Store(false)
	pumpActions(t, manager, false, func() bool { return !old.stopping.IsZero() })
	if old.scope.runs["health"][0].failures != 3 {
		t.Fatal("wrong consecutive failure threshold")
	}
	healthy.Store(true)
	pumpActions(t, manager, false, func() bool {
		for child := range manager.workers {
			if child != old && child.ready {
				return true
			}
		}
		return false
	})
}

func TestUnawaitedAndCancelledActions(t *testing.T) {
	app := actionApp(t, "async")
	command := actionCommand(t)
	command.Env["ACTION_DELAY"] = "5s"
	command.Timeout = 6 * time.Second
	wait := false
	app.Actions = map[string]Action{"slow": command}
	app.Triggers = map[string][]Trigger{"pre_start": {{Action: "slow", Wait: &wait}}}
	manager := actionManager(t, app)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	if manager.actionTasks != 1 {
		t.Fatal("unawaited hook delayed startup")
	}
	started := time.Now()
	for child := range manager.workers {
		manager.stop(child, time.Now(), "cancel in flight")
	}
	pumpActions(t, manager, true, func() bool { return len(manager.workers) == 0 && manager.actionTasks == 0 })
	if time.Since(started) > 2*time.Second {
		t.Fatal("cancel did not interrupt command")
	}
}

func TestActionStartFailureAndStopDeadline(t *testing.T) {
	t.Run("pre_start prevents launch", func(t *testing.T) {
		app := actionApp(t, "blocked")
		command := actionCommand(t)
		command.Env["ACTION_EXIT"] = "5"
		app.Actions = map[string]Action{"fail": command}
		app.Triggers = map[string][]Trigger{"pre_start": {{Action: "fail"}}}
		manager := actionManager(t, app)
		pumpActions(t, manager, false, func() bool { return manager.services[app.Name].failures > 0 })
		for child := range manager.workers {
			if child.pid != 0 {
				t.Fatal("spawned despite pre_start failure")
			}
		}
		if manager.services[app.Name].retryAt.Before(time.Now()) {
			t.Fatal("missing failure backoff")
		}
	})
	t.Run("pre_stop is bounded", func(t *testing.T) {
		app := actionApp(t, "stopping")
		command := actionCommand(t)
		command.Env["ACTION_DELAY"] = "5s"
		command.Timeout = 6 * time.Second
		app.Actions = map[string]Action{"slow": command}
		app.Triggers = map[string][]Trigger{"pre_stop": {{Action: "slow"}}}
		manager := actionManager(t, app)
		pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
		started := time.Now()
		for child := range manager.workers {
			manager.stop(child, started, "deadline")
		}
		pumpActions(t, manager, true, func() bool { return len(manager.workers) == 0 && manager.actionTasks == 0 })
		if elapsed := time.Since(started); elapsed < app.StopTimeout || elapsed > 2*time.Second {
			t.Fatalf("stop hook escaped deadline: %s", elapsed)
		}
	})
}

func TestActionInheritedEndpoint(t *testing.T) {
	app := actionApp(t, "endpoint")
	delete(app.Env, "TEST_OOTH_VIRTUAL")
	app.Listen = Socket{Network: "tcp", Address: "127.0.0.1:0"}
	marker := filepath.Join(t.TempDir(), "endpoint")
	command := actionCommand(t)
	command.Env["ACTION_MARKER"] = marker
	command.Env["ACTION_TEXT"] = "{{.listen.address}}"
	confirmed := actionCommand(t)
	confirmed.Env["ACTION_MARKER"] = marker + ".ready"
	app.Actions = map[string]Action{
		"record":    command,
		"confirmed": confirmed,
		"probe":     {TCP: &SocketCheck{Address: "{{.listen.address}}", Send: "0ms check\n"}, Expect: Expectation{Contains: "check"}},
	}
	app.Triggers = map[string][]Trigger{"post_start": {{Action: "record"}}, "readiness": {{Action: "probe"}}, "health": {{Action: "confirmed", Interval: time.Hour}}}
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: [app.yaml]\n"+unlimitedTestResources)
	writeApp(t, filepath.Join(directory, "app.yaml"), app)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
	waitRestart(t, "endpoint action not executed", func() bool { _, err := os.Stat(marker); return err == nil })
	waitRestart(t, "inherited listener readiness did not pass", func() bool { _, err := os.Stat(marker + ".ready"); return err == nil })
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimSpace(string(data))
	if strings.HasSuffix(address, ":0") {
		t.Fatal("template used configured rather than bound port")
	}
	request(t, "tcp", address, "0ms actual-port")
}

func TestHealthLogRecoveryWithoutRestart(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			writer.WriteHeader(503)
		}
	}))
	defer server.Close()
	app := actionApp(t, "log-only")
	app.Actions = map[string]Action{"probe": {HTTP: &HTTPRequest{URL: server.URL}}}
	app.Triggers = map[string][]Trigger{"health": {{Action: "probe", Scope: "app", Interval: 20 * time.Millisecond, FailureThreshold: 2, OnFailure: "log"}}}
	manager := actionManager(t, app)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	var child *process
	for worker := range manager.workers {
		child = worker
	}
	healthy.Store(false)
	pumpActions(t, manager, false, func() bool { return !child.ready })
	if !child.stopping.IsZero() {
		t.Fatal("log-only health failure restarted worker")
	}
	healthy.Store(true)
	pumpActions(t, manager, false, func() bool { return child.ready })
	if !child.stopping.IsZero() || child.exited {
		t.Fatal("health recovery replaced worker")
	}
}
