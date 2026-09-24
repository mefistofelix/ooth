package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLaunchTemplates(t *testing.T) {
	t.Setenv("TEST_TEMPLATE_PARENT", "parent {{not_recursive}}")
	app := App{Name: "web", Directory: t.TempDir(), Source: "app.yaml",
		Command: []string{"worker", "--bind={{.listen.url}}", "{{.env.TEST_TEMPLATE_PARENT}}", "$HOME; echo literal"},
		Env:     map[string]string{"ADDRESS": "{{.listen.address}}", "NAME": "{{.name}}"}, Listen: Socket{"tcp6", "[::1]:1234"}}
	command, environment, err := app.expandLaunch("[::1]:4567")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"worker", "--bind=tcp://[::1]:4567", "parent {{not_recursive}}", "$HOME; echo literal"}
	if !reflect.DeepEqual(command, want) || environment["ADDRESS"] != "[::1]:4567" || environment["NAME"] != "web" {
		t.Fatalf("command=%q environment=%v", command, environment)
	}
	if app.Command[1] != "--bind={{.listen.url}}" || app.Env["NAME"] != "{{.name}}" {
		t.Fatal("expansion mutated configuration")
	}
	app.Listen = Socket{"unix", "/tmp/ooth socket"}
	app.Command = []string{"worker", "{{.listen.url}}", "{{.listen.path}}"}
	command, _, err = app.expandLaunch(app.Listen.Address)
	if err != nil || command[1] != "unix:///tmp/ooth%20socket" || command[2] != app.Listen.Address {
		t.Fatalf("Unix command=%q error=%v", command, err)
	}
	app.Listen.Address = filepath.Join(app.Directory, "local socket")
	command, _, err = app.expandLaunch(app.Listen.Address)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(command[1])
	if err != nil || uri.Scheme != "unix" || uri.Host != "" || !strings.HasSuffix(uri.Path, filepath.ToSlash(app.Listen.Address)) {
		t.Fatalf("native path encoded as a host instead of URL path: %q, %v", command[1], err)
	}
	for _, value := range []string{"{{.missing}}", "{{.env.TEST_TEMPLATE_MISSING}}", "{{.listen.port}}", "{{", "{{printf \"\\x00\"}}"} {
		app.Command = []string{"worker", value}
		if _, _, err := app.expandLaunch(app.Listen.Address); err == nil {
			t.Errorf("accepted invalid/unavailable template %q", value)
		}
	}
	app.Listen = Socket{}
	app.Vars = map[string]string{"endpoint": "tcp://127.0.0.1:8080"}
	app.Command = []string{"{{.directory}}/worker", "{{.name}}", "{{.vars.endpoint}}"}
	app.Env = map[string]string{"VALUE": "{{.env.TEST_TEMPLATE_PARENT}}", "BIND": "{{.vars.endpoint}}"}
	command, environment, err = app.expandLaunch("")
	if err != nil || command[0] != filepath.Join(app.Directory, "worker") || environment["VALUE"] != want[2] || command[2] != app.Vars["endpoint"] || environment["BIND"] != command[2] {
		t.Fatalf("virtual command=%q environment=%v error=%v", command, environment, err)
	}
}

type launchReport struct {
	PID     int
	Args    []string
	Value   string
	Address string
}

func TestLaunchTemplateHelper(t *testing.T) {
	directory := os.Getenv("TEST_TEMPLATE_REPORT")
	if directory == "" {
		return
	}
	address := ""
	if os.Getenv("OOTH_LISTEN_HANDLE") != "" {
		listener, err := testWorkerListener()
		if err != nil {
			os.Exit(2)
		}
		defer listener.Close()
		address = listener.Addr().String()
	}
	report := launchReport{os.Getpid(), os.Args[3:], os.Getenv("TEST_TEMPLATE_VALUE"), address}
	data, _ := json.Marshal(report)
	if err := os.WriteFile(filepath.Join(directory, strconv.Itoa(report.PID)+".json"), data, 0600); err != nil {
		os.Exit(3)
	}
	WriteEvent(os.Stdout, Event{Type: "ready", Time: time.Now().UnixNano()})
	if os.Getenv("TEST_TEMPLATE_WORK") == "1" {
		go func() {
			started := time.Now()
			WriteEvent(os.Stdout, Event{Type: "start", ID: "job", Time: started.UnixNano()})
			time.Sleep(500 * time.Millisecond)
			WriteEvent(os.Stdout, Event{Type: "end", ID: "job", Time: time.Now().UnixNano(), DurationNS: time.Since(started).Nanoseconds()})
		}()
	}
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() && strings.HasPrefix(scanner.Text(), "v=1 event=stop ts=") {
		os.Exit(0)
	}
	os.Exit(4)
}

// Exercises the generic process pool on both OSes, independently of whether a
// particular external runtime supports reuseport. This job opens no socket.
func TestPoolWithoutListener(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['app.yaml']\nresources: {max_cpu_percent: 0, min_available_memory_percent: 0}\n")
	app := App{Name: "jobs", MinWorkers: 1, MaxWorkers: 2, Ready: "event",
		Command:     []string{os.Args[0], "-test.run=^TestLaunchTemplateHelper$", "--"},
		Env:         map[string]string{"TEST_TEMPLATE_REPORT": directory, "TEST_TEMPLATE_WORK": "1"},
		Concurrency: 1, ScaleAt: 80, ScaleWindow: 50 * time.Millisecond,
		IdleTimeout: 150 * time.Millisecond, StartTimeout: 2 * time.Second, StopTimeout: time.Second}
	writeApp(t, filepath.Join(directory, "app.yaml"), app)
	log := &proxyLog{apps: make(map[string]proxyWorkerState)}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(log)) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		state := log.state("jobs")
		if state.live != 0 || state.starts != 2 || state.ends != 2 {
			t.Errorf("unclean generic pool: %+v", state)
		}
	}()
	for {
		state := log.state("jobs")
		if state.spawned == 2 && state.ends == 2 && state.live == 1 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("pool did not grow and return to minimum: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	files, _ := filepath.Glob(filepath.Join(directory, "*.json"))
	if len(files) != 2 {
		t.Fatalf("expected two job reports: %v", files)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var report launchReport
		if err := json.Unmarshal(data, &report); err != nil || report.Address != "" {
			t.Fatalf("unexpected inherited socket: %+v, %v", report, err)
		}
	}
	// The retained minimum must outlive another idle interval.
	time.Sleep(2 * app.IdleTimeout)
	if state := log.state("jobs"); state.live != 1 || state.spawned != 2 {
		t.Fatalf("minimum was stopped/replaced: %+v", state)
	}
}

func TestLaunchTemplatesAcrossRestart(t *testing.T) {
	t.Setenv("TEST_TEMPLATE_PARENT", "value with spaces; $literal")
	for _, kind := range []string{"init", "socket"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "ooth.yaml")
			app := App{Name: kind, Startup: true, Ready: "event", Command: []string{os.Args[0], "-test.run=^TestLaunchTemplateHelper$", "--", "{{.name}}", "{{.env.TEST_TEMPLATE_PARENT}}"},
				Env:        map[string]string{"TEST_TEMPLATE_REPORT": "{{.directory}}", "TEST_TEMPLATE_VALUE": "{{.source}}"},
				MaxWorkers: 1, Concurrency: 1, IdleTimeout: time.Minute, StartTimeout: 3 * time.Second,
				StopTimeout: time.Second, ScaleAt: 80, ScaleWindow: time.Second}
			if kind == "socket" {
				app.Listen = Socket{"tcp", "127.0.0.1:0"}
				app.Command = append(app.Command, "{{.listen.url}}")
			}
			write(t, path, "watch: ['app.yaml']\n")
			writeApp(t, filepath.Join(directory, "app.yaml"), app)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, path, slog.Default()) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			for instance := 1; instance <= 2; instance++ {
				var files []string
				for len(files) < instance && ctx.Err() == nil {
					files, _ = filepath.Glob(filepath.Join(directory, "*.json"))
					time.Sleep(10 * time.Millisecond)
				}
				if len(files) != instance {
					t.Fatal("missing process startup/restart report")
				}
				var latest launchReport
				for _, file := range files {
					data, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					var report launchReport
					if err := json.Unmarshal(data, &report); err != nil {
						t.Fatal(err)
					}
					if report.Args[0] != kind || report.Args[1] != os.Getenv("TEST_TEMPLATE_PARENT") || report.Value != filepath.Join(directory, "app.yaml") {
						t.Fatalf("wrong argv/environment: %+v", report)
					}
					if kind == "socket" {
						_, port, err := net.SplitHostPort(report.Address)
						if err != nil || port == "0" || report.Args[2] != "tcp://"+report.Address {
							t.Fatalf("wrong bound endpoint: %+v", report)
						}
					}
					latest = report
				}
				if instance == 1 {
					process, err := os.FindProcess(latest.PID)
					if err != nil {
						t.Fatal(err)
					}
					if err := process.Kill(); err != nil {
						t.Fatal(err)
					}
					process.Release()
				}
			}
		})
	}
}
