package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleCalendar(t *testing.T) {
	for _, test := range []struct {
		expression string
		at         string
		match      bool
	}{
		{"*/15 9-17 * * MON-FRI", "2026-10-02T09:30:00Z", true},
		{"*/15 9-17 * * MON-FRI", "2026-10-03T09:30:00Z", false},
		{"*/15 9-17 * * MON-FRI", "2026-10-02T09:31:00Z", false},
		{"5,20-30/5 1,13 * jan,oct *", "2026-10-02T13:25:00Z", true},
		{"5,20-30/5 1,13 * jan,oct *", "2026-11-02T13:25:00Z", false},
		{"0 0 29 feb *", "2028-02-29T00:00:00Z", true},
		{"0 0 29 feb *", "2027-02-28T00:00:00Z", false},
		{"0 0 1 * mon", "2026-10-05T00:00:00Z", true}, // Restricted days use OR.
		{"0 0 1 * mon", "2026-10-01T00:00:00Z", true},
		{"0 0 1 * mon", "2026-10-02T00:00:00Z", false},
		{"0 0 */2 * mon", "2026-10-05T00:00:00Z", true}, // Wildcard days use AND.
		{"0 0 */2 * mon", "2026-10-12T00:00:00Z", false},
		{"0 0 * * 7", "2026-10-04T00:00:00Z", true},
		{"0 0 * * 0", "2026-10-04T00:00:00Z", true},
		{"@weekly", "2026-10-04T00:00:00Z", true},
		{"@hourly", "2026-10-02T13:00:00Z", true},
		{"@daily", "2026-10-02T13:00:00Z", false},
		{"@monthly", "2026-11-01T00:00:00Z", true},
		{"@yearly", "2027-01-01T00:00:00Z", true},
	} {
		t.Run(test.expression+test.at, func(t *testing.T) {
			plan, err := parseSchedule(test.expression)
			if err != nil {
				t.Fatal(err)
			}
			at, _ := time.Parse(time.RFC3339, test.at)
			if got := plan.matches(at); got != test.match {
				t.Fatalf("matches = %v, want %v", got, test.match)
			}
		})
	}
	for _, expression := range []string{"0s", "-5m", "never", "* * * *", "0 * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "*/-2 * * * *", "5/2 * * * *", "9-2 * * * *", "1,,2 * * * *", "* * * foo *", "* * * * monday", "* * * * ?", "+1 * * * *", "*/999999999999999999999999 * * * *"} {
		if _, err := parseSchedule(expression); err == nil {
			t.Errorf("accepted %q", expression)
		}
	}
	for _, expression := range []string{"10s", "5m", "1h30m"} {
		plan, err := parseSchedule(expression)
		if err != nil || plan.interval <= 0 {
			t.Fatalf("interval %q: %+v %v", expression, plan, err)
		}
	}
}

func TestScheduleCadenceAndClock(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 12, 0, time.UTC)
	plan, _ := parseSchedule("10s")
	current := &service{name: "job", config: App{Schedule: "10s", MaxWorkers: 1}, schedule: plan, nextRun: plan.next(start)}
	manager := actionManager(t)
	manager.scheduleDue(current, start.Add(9*time.Second))
	if current.scheduled {
		t.Fatal("interval ran before its first deadline")
	}
	manager.scheduleDue(current, start.Add(35*time.Second))
	if !current.scheduled || !current.nextRun.Equal(start.Add(40*time.Second)) {
		t.Fatalf("missed ticks were queued or changed cadence: %+v", current)
	}
	current.scheduled = false
	manager.scheduleDue(current, start.Add(36*time.Second))
	if current.scheduled {
		t.Fatal("catch-up execution")
	}
	plan, _ = parseSchedule("* * * * *")
	current.schedule, current.nextRun = plan, plan.next(start)
	manager.scheduleDue(current, start.Add(47*time.Second))
	if current.scheduled {
		t.Fatal("cron ran in its registration minute")
	}
	manager.scheduleDue(current, start.Add(48*time.Second))
	if !current.scheduled {
		t.Fatal("cron missed its minute boundary")
	}
	current.scheduled = false
	manager.scheduleDue(current, start.Add(49*time.Second))
	if current.scheduled {
		t.Fatal("cron fired twice in the same minute")
	}
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = parseSchedule("30 1 * * *")
	for _, stamp := range []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"} {
		at, _ := time.Parse(time.RFC3339, stamp)
		if !plan.matches(at.In(location)) {
			t.Fatal("repeated daylight-saving hour did not match", stamp)
		}
	}
}

func TestScheduleConfiguration(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "root.yaml")
	appPath := filepath.Join(directory, "job", "ooth.yaml")
	write(t, root, "watch: ['job/ooth.yaml']\n")
	for _, extra := range []string{"schedule: '10s'\nmax_exec_time: 5m\n", "schedule: '*/5 * * * *'\noverlap: true\nmax_workers: 3\n"} {
		write(t, appPath, "command: [job]\n"+extra)
		snapshot, err := Load(root)
		if err != nil {
			t.Fatal(err)
		}
		app := snapshot.Apps["job"]
		if app.Schedule == "10s" && (app.Overlap || app.MaxExecTime != 5*time.Minute) {
			t.Fatalf("defaults/limit: %+v", app)
		}
	}
	for _, extra := range []string{"schedule: '0s'", "schedule: '* * * *'", "schedule: '10s'\nstartup: true", "schedule: '10s'\nmin_workers: 1", "schedule: '10s'\nlisten: {network: tcp, address: '127.0.0.1:0'}", "overlap: true", "max_exec_time: -1s"} {
		write(t, appPath, "command: [job]\n"+extra+"\n")
		if _, err := Load(root); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
	if err := validateDependencies(map[string]App{"service": {Requires: []string{"job"}}, "job": {Schedule: "10s"}}); err == nil {
		t.Fatal("accepted a scheduled job as a persistent service dependency")
	}
}

func TestScheduledHelper(t *testing.T) {
	if os.Getenv("TEST_SCHEDULE_JOB") != "1" {
		return
	}
	marker := func(event string) {
		file, err := os.OpenFile(os.Getenv("SCHEDULE_MARKER"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(90)
		}
		fmt.Fprintf(file, "%s %d\n", event, os.Getpid())
		file.Close()
	}
	marker("start")
	WriteEvent(os.Stdout, Event{Type: "ready", Time: time.Now().UnixNano()})
	stop := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "event=stop") {
				marker("stop")
				if os.Getenv("SCHEDULE_IGNORE_STOP") != "1" {
					stop <- struct{}{}
					return
				}
			}
		}
	}()
	if os.Getenv("SCHEDULE_IGNORE_STOP") == "1" {
		signal.Ignore(StopSignal)
	}
	duration, _ := time.ParseDuration(os.Getenv("SCHEDULE_DURATION"))
	select {
	case <-time.After(duration):
	case <-stop:
		drain, _ := time.ParseDuration(os.Getenv("SCHEDULE_DRAIN"))
		time.Sleep(drain)
	}
	marker("end")
	code, _ := strconv.Atoi(os.Getenv("SCHEDULE_EXIT"))
	os.Exit(code)
}

func scheduledApp(t *testing.T) App {
	app := actionApp(t, "job")
	app.Command = []string{os.Args[0], "-test.run=^TestScheduledHelper$"}
	app.MinWorkers = 0
	app.Schedule = "1h"
	app.Env = map[string]string{"TEST_SCHEDULE_JOB": "1", "SCHEDULE_DURATION": "10s", "SCHEDULE_MARKER": filepath.Join(t.TempDir(), "events")}
	return app
}

func scheduledManager(t *testing.T, apps ...App) *manager {
	manager := actionManager(t, apps...)
	for _, service := range manager.services {
		plan, err := parseSchedule(service.config.Schedule)
		if err != nil {
			t.Fatal(err)
		}
		service.schedule, service.nextRun = plan, plan.next(time.Now())
	}
	return manager
}

func fireScheduled(manager *manager, name string) {
	manager.services[name].nextRun = time.Now()
	manager.tick(time.Now(), false)
}

func TestScheduledNoOverlapAndDrain(t *testing.T) {
	app := scheduledApp(t)
	app.Env["SCHEDULE_DRAIN"] = "120ms"
	manager := scheduledManager(t, app)
	service := manager.services[app.Name]
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(service) == 1 })
	var old *process
	for child := range manager.workers {
		old = child
	}
	fireScheduled(manager, app.Name)
	if len(manager.workers) != 1 || service.scheduled {
		t.Fatal("overlap despite default policy")
	}
	manager.stop(old, time.Now(), "test draining")
	fireScheduled(manager, app.Name)
	if len(manager.workers) != 1 || service.scheduled {
		t.Fatal("draining execution no longer counted")
	}
	pumpActions(t, manager, false, func() bool { return len(manager.workers) == 0 })
	manager.tick(time.Now(), false)
	if len(manager.workers) != 0 || service.scheduled {
		t.Fatal("skipped deadline was replayed")
	}
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(service) == 1 })
	for child := range manager.workers {
		if child == old {
			t.Fatal("next execution reused old process")
		}
	}
}

func TestScheduledOverlapLimitAndHooks(t *testing.T) {
	app := scheduledApp(t)
	app.Overlap, app.MaxWorkers = true, 2
	var starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { starts.Add(1) }))
	defer server.Close()
	app.Actions = map[string]Action{"hook": {HTTP: &HTTPRequest{URL: server.URL}}}
	app.Triggers = map[string][]Trigger{"pre_start": {{Action: "hook", Scope: "app"}}}
	manager := scheduledManager(t, app)
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 2 })
	fireScheduled(manager, app.Name)
	if len(manager.workers) != 2 || starts.Load() != 2 || manager.services[app.Name].scheduled {
		t.Fatalf("overlap limit/hooks: workers=%d hooks=%d", len(manager.workers), starts.Load())
	}
}

func TestScheduledMaximumExecutionTime(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprint(ignore), func(t *testing.T) {
			app := scheduledApp(t)
			app.MaxExecTime, app.StopTimeout = 150*time.Millisecond, 150*time.Millisecond
			if ignore {
				app.Env["SCHEDULE_IGNORE_STOP"] = "1"
			}
			manager := scheduledManager(t, app)
			fireScheduled(manager, app.Name)
			pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
			var child *process
			for current := range manager.workers {
				child = current
			}
			pumpActions(t, manager, false, func() bool { return len(manager.workers) == 0 })
			if child.killed != ignore || child.stopping.Sub(child.executed) < app.MaxExecTime {
				t.Fatalf("timeout/kill: killed=%v elapsed=%v", child.killed, child.stopping.Sub(child.executed))
			}
			data, err := os.ReadFile(app.Env["SCHEDULE_MARKER"])
			if err != nil || !strings.Contains(string(data), "stop ") || (!ignore && !strings.Contains(string(data), "end ")) {
				t.Fatalf("graceful command/drain: %s %v", data, err)
			}
			manager.tick(time.Now(), false)
			if len(manager.workers) != 0 {
				t.Fatal("timed-out job restarted before next deadline")
			}
		})
	}
}

func TestScheduledNaturalExitAndFailure(t *testing.T) {
	for _, code := range []string{"0", "7"} {
		t.Run(code, func(t *testing.T) {
			app := scheduledApp(t)
			app.Env["SCHEDULE_DURATION"], app.Env["SCHEDULE_EXIT"] = "40ms", code
			manager := scheduledManager(t, app)
			fireScheduled(manager, app.Name)
			var child *process
			for current := range manager.workers {
				child = current
			}
			pumpActions(t, manager, false, func() bool { return len(manager.workers) == 0 })
			if strconv.Itoa(child.exitCode) != code {
				t.Fatalf("exit code %d", child.exitCode)
			}
			for i := 0; i < 5; i++ {
				manager.tick(time.Now().Add(time.Second), false)
			}
			if len(manager.workers) != 0 || manager.services[app.Name].demand {
				t.Fatal("natural/failed job exit became continuous restart")
			}
		})
	}
}

func TestScheduledDependencyAndReadiness(t *testing.T) {
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			writer.WriteHeader(503)
		}
	}))
	defer server.Close()
	dependency := actionApp(t, "database")
	dependency.MinWorkers = 0
	dependency.Actions = map[string]Action{"ready": {HTTP: &HTTPRequest{URL: server.URL}}}
	dependency.Triggers = map[string][]Trigger{"readiness": {{Action: "ready", Interval: 10 * time.Millisecond, OnFailure: "log", FailureThreshold: 100}}}
	app := scheduledApp(t)
	app.Requires = []string{dependency.Name}
	manager := scheduledManager(t, dependency, app)
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return len(manager.services[dependency.Name].workers) == 1 })
	for i := 0; i < 5; i++ {
		fireScheduled(manager, app.Name)
	}
	if len(manager.services[app.Name].workers) != 0 || !manager.services[app.Name].scheduled {
		t.Fatal("readiness gate bypassed or lost pending execution")
	}
	healthy.Store(true)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	if len(manager.services[app.Name].workers) != 1 {
		t.Fatal("pending deadlines accumulated")
	}
}

func TestScheduledRunInterval(t *testing.T) {
	app := scheduledApp(t)
	app.Schedule, app.Env["SCHEDULE_DURATION"] = "100ms", "180ms"
	directory := t.TempDir()
	root := filepath.Join(directory, "root.yaml")
	write(t, root, "watch: ['job/ooth.yaml']\n"+unlimitedTestResources)
	writeApp(t, filepath.Join(directory, "job", "ooth.yaml"), app)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := actionManager(t)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, root, manager.log) }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(app.Env["SCHEDULE_MARKER"])
		if strings.Count(string(data), "end ") >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled supervisor did not stop")
	}
	data, _ := os.ReadFile(app.Env["SCHEDULE_MARKER"])
	active, completed := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "start ") {
			active++
			if active > 1 {
				t.Fatal("real interval scheduler overlapped executions")
			}
		} else if strings.HasPrefix(line, "end ") {
			active--
			completed++
		}
	}
	if completed < 3 || active != 0 {
		t.Fatalf("incomplete interval executions: %s", data)
	}
}

func TestScheduledReservationsAndAwaitedCleanup(t *testing.T) {
	for _, event := range []string{"pre_start", "post_stop"} {
		t.Run(event, func(t *testing.T) {
			started := make(chan struct{}, 1)
			ctx, release := context.WithCancel(context.Background())
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				started <- struct{}{}
				select {
				case <-ctx.Done():
				case <-request.Context().Done():
				}
			}))
			defer server.Close()
			defer release()
			app := scheduledApp(t)
			app.MaxExecTime = 100 * time.Millisecond
			app.Actions = map[string]Action{"wait": {HTTP: &HTTPRequest{URL: server.URL}, Timeout: 3 * time.Second}}
			app.Triggers = map[string][]Trigger{event: {{Action: "wait", Scope: "app"}}}
			manager := scheduledManager(t, app)
			fireScheduled(manager, app.Name)
			var child *process
			for current := range manager.workers {
				child = current
			}
			pumpActions(t, manager, false, func() bool { return len(started) != 0 })
			fireScheduled(manager, app.Name)
			if len(manager.workers) != 1 || manager.services[app.Name].scheduled {
				t.Fatal("overlap during reservation/post-stop hook")
			}
			if event == "pre_start" {
				manager.tick(child.started.Add(app.MaxExecTime+time.Millisecond), false)
				if !child.pending || !child.executed.IsZero() || !child.stopping.IsZero() {
					t.Fatal("execution limit incorrectly includes pre-start action")
				}
			}
			release()
			pumpActions(t, manager, false, func() bool { return len(manager.workers) == 0 })
		})
	}
}

func TestScheduledReloadKeepsNonOverlap(t *testing.T) {
	app := scheduledApp(t)
	app.Env["SCHEDULE_DRAIN"] = "150ms"
	manager := scheduledManager(t, app)
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	app.Schedule = "2h"
	if err := manager.apply(Snapshot{Apps: map[string]App{app.Name: app}}); err != nil {
		t.Fatal(err)
	}
	current := manager.services[app.Name]
	if current.schedule.interval != 2*time.Hour || current.nextRun.IsZero() {
		t.Fatal("reload did not install new schedule")
	}
	fireScheduled(manager, app.Name)
	if current.scheduled || len(manager.workers) != 1 {
		t.Fatal("reload ignored old draining execution")
	}
	pumpActions(t, manager, false, func() bool { return len(manager.workers) == 0 })
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(current) == 1 })
}

func TestScheduledNoGrowthAndShutdown(t *testing.T) {
	app := scheduledApp(t)
	app.Schedule, app.ScaleWindow = "* * * * *", 10*time.Millisecond
	manager := scheduledManager(t, app)
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	var child *process
	for current := range manager.workers {
		child = current
	}
	now := time.Now()
	manager.handle(message{process: child, event: Event{Type: "start", ID: "busy"}}, now)
	manager.tick(now.Add(20*time.Millisecond), false)
	if len(manager.workers) != 1 {
		t.Fatal("telemetry saturation started an unscheduled execution")
	}
	manager.services[app.Name].nextRun = time.Now()
	pumpActions(t, manager, true, func() bool { return len(manager.workers) == 0 })
	manager.tick(time.Now(), true)
	if len(manager.workers) != 0 || manager.services[app.Name].scheduled {
		t.Fatal("shutdown triggered another scheduled execution")
	}
}

func TestScheduledRemoveAndReAddKeepsNonOverlap(t *testing.T) {
	app := scheduledApp(t)
	app.Env["SCHEDULE_DRAIN"] = "150ms"
	manager := scheduledManager(t, app)
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(manager.services[app.Name]) == 1 })
	if err := manager.apply(Snapshot{Apps: map[string]App{}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.apply(Snapshot{Apps: map[string]App{app.Name: app}}); err != nil {
		t.Fatal(err)
	}
	current := manager.services[app.Name]
	fireScheduled(manager, app.Name)
	if current.scheduled || len(current.workers) != 0 || len(manager.workers) != 1 {
		t.Fatal("re-added app overlapped its removed, draining execution")
	}
	pumpActions(t, manager, false, func() bool { return len(manager.workers) == 0 })
	fireScheduled(manager, app.Name)
	pumpActions(t, manager, false, func() bool { return readyWorkers(current) == 1 })
}

func TestScheduledLaunchFailure(t *testing.T) {
	app := scheduledApp(t)
	app.Command = []string{filepath.Join(t.TempDir(), "missing-executable")}
	manager := scheduledManager(t, app)
	fireScheduled(manager, app.Name)
	current := manager.services[app.Name]
	if len(manager.workers) != 0 || current.scheduled || current.demand || current.failures != 0 {
		t.Fatal("failed launch became pending/retry demand")
	}
	manager.tick(time.Now().Add(time.Second), false)
	if len(manager.workers) != 0 || current.scheduled || current.demand {
		t.Fatal("failed launch was retried before its next deadline")
	}
}
