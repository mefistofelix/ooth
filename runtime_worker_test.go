package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every positive case runs the real supervisor: the harness never starts a
// worker itself. External runtimes are opt-in and remain outside the Go module.
func TestRuntimeLifecycle(t *testing.T) {
	for _, name := range []string{"PYTHON", "NODE", "BUN", "DENO", "SOCKETIFY", "TRUEASYNC"} {
		protocols := []string{"http1"}
		if name == "NODE" || name == "BUN" || name == "DENO" || name == "TRUEASYNC" {
			protocols = append(protocols, "h2c")
		}
		for _, protocol := range protocols {
			for _, proxy := range []string{"direct", "caddy", "nginx"} {
				t.Run(name+"/"+protocol+"/"+proxy, func(t *testing.T) {
					executable := os.Getenv("OOTH_TEST_" + name)
					if executable == "" {
						t.Skip("set OOTH_TEST_" + name)
					}
					executable, _ = filepath.Abs(executable)
					workerEnv := map[string]string{}
					var command []string
					script, _ := filepath.Abs("tools/probes/node_worker.cjs")
					switch name {
					case "PYTHON":
						script, _ = filepath.Abs("examples/worker.py")
					case "SOCKETIFY":
						script, _ = filepath.Abs("tools/probes/socketify/worker.py")
						workerEnv["TEST_OOTH_PROTOCOL"] = "1"
					case "TRUEASYNC":
						if runtime.GOOS != "windows" {
							t.Skip("official Linux TrueAsync 0.10.0 is static without FFI; user chose no rebuild")
						}
						script, _ = filepath.Abs("tools/probes/trueasync/worker.php")
						command = []string{executable, "-n", "-d", "display_errors=stderr"}
						command = append(command, "-d", "extension_dir="+filepath.Join(filepath.Dir(executable), "ext"), "-d", "extension=php_true_async_server.dll", "-d", "extension=php_ffi.dll", "-d", "ffi.enable=1")
					}
					if command == nil {
						command = []string{executable}
						if name == "DENO" {
							command = append(command, "run", "-A")
						}
					}
					command = append(command, script)
					if protocol == "h2c" {
						workerEnv["TEST_NODE_H2C"] = "1"
					}
					runRuntimeLifecycle(t, name, protocol, proxy, command, workerEnv, true)
				})
			}
		}
	}
}

func runRuntimeLifecycle(t *testing.T, name, protocol, proxy string, command []string, workerEnv map[string]string, inherited bool) {
	t.Helper()
	proxyExecutable := os.Getenv("OOTH_TEST_" + strings.ToUpper(proxy))
	if proxy != "direct" && proxyExecutable == "" {
		t.Skip("set OOTH_TEST_" + strings.ToUpper(proxy))
	}
	directory := t.TempDir()
	if root := os.Getenv("OOTH_TEST_ARTIFACTS"); root != "" {
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
		var err error
		directory, err = os.MkdirTemp(root, strings.ToLower(name)+"-"+protocol+"-"+proxy+"-")
		if err != nil {
			t.Fatal(err)
		}
	}
	address := freeAddress(t)
	app := App{Name: "worker", Command: command, Env: workerEnv, Listen: Socket{"tcp", address}, Ready: "event", MaxWorkers: 2, Concurrency: 1, ScaleAt: 80, ScaleWindow: 100 * time.Millisecond, IdleTimeout: 1500 * time.Millisecond, StartTimeout: 5 * time.Second, StopTimeout: 3 * time.Second}
	if !inherited {
		app.Listen = Socket{}
		app.MinWorkers = 1
		app.Vars = map[string]string{"endpoint": "tcp://" + address}
	}
	if proxy != "direct" {
		app.Requires = []string{"proxy"}
	}
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['apps/*.yaml']\nresources: {max_cpu_percent: 0, min_available_memory_percent: 0}\n")
	writeApp(t, filepath.Join(directory, "apps", "worker.yaml"), app)
	requestAddress, prefix := address, ""
	if proxy != "direct" {
		proxyExecutable, _ = filepath.Abs(proxyExecutable)
		root := filepath.ToSlash(directory)
		requestAddress = freeAddress(t)
		addresses := map[string]string{"Root": root, "Address": requestAddress, "Python": address, "Node": address, "PHP": address, "FreshConnections": "1"}
		write(t, filepath.Join(directory, "health"), "ready")
		for _, folder := range []string{"logs", "temp"} {
			if err := os.MkdirAll(filepath.Join(directory, folder), 0755); err != nil {
				t.Fatal(err)
			}
		}
		config := filepath.Join(directory, "Caddyfile")
		proxyCommand := []string{proxyExecutable, "run", "--config", config, "--adapter", "caddyfile"}
		if proxy == "nginx" {
			config = filepath.Join(directory, "nginx.conf")
			proxyCommand = []string{proxyExecutable, "-p", root + "/", "-c", config}
			renderProxyFixture(t, "nginx.conf.tmpl", config, addresses)
		} else {
			renderProxyFixture(t, "Caddyfile.tmpl", config, addresses)
		}
		renderProxyFixture(t, "webserver.yaml.tmpl", filepath.Join(directory, "apps", "proxy.yaml"), map[string]any{"Command": proxyCommand, "Root": root})
		prefix = "/python"
		if protocol == "h2c" {
			prefix = "/node"
		}
	}
	log := &proxyLog{apps: make(map[string]proxyWorkerState)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(log)) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Error("supervisor shutdown timed out")
		}
		log.mu.Lock()
		lines := strings.Join(log.lines, "\n")
		log.mu.Unlock()
		os.WriteFile(filepath.Join(directory, "ooth.log"), []byte(lines+"\n"), 0600)
		state := log.state("worker")
		if state.live != 0 || state.starts != state.ends || state.telemetry != state.spawned || strings.Contains(lines, "worker exceeded graceful timeout") {
			t.Errorf("unclean worker lifecycle: %+v", state)
		}
		if t.Failed() {
			t.Log(lines)
		}
	}()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(proxy != "direct" || protocol != "h2c")
	protocols.SetUnencryptedHTTP2(proxy == "direct" && protocol == "h2c")
	transport := &http.Transport{Protocols: protocols, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 7 * time.Second}
	type reply struct {
		body string
		err  error
	}
	fetch := func(path string) reply {
		response, err := client.Get("http://" + requestAddress + path)
		if err != nil {
			return reply{err: err}
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err == nil && response.StatusCode != 200 {
			err = fmt.Errorf("HTTP %d: %s", response.StatusCode, body)
		}
		if err == nil && proxy == "direct" && protocol == "h2c" && response.ProtoMajor != 2 {
			err = fmt.Errorf("expected h2c, received %s", response.Proto)
		}
		return reply{string(body), err}
	}
	check := func(result reply) string {
		t.Helper()
		if result.err != nil || !strings.HasPrefix(result.body, "worker=") {
			t.Fatalf("reply=%q error=%v", result.body, result.err)
		}
		if name != "PYTHON" && !strings.Contains(result.body, "protocol="+protocol) {
			t.Fatalf("expected upstream %s: %q", protocol, result.body)
		}
		return strings.Fields(result.body)[0]
	}
	wait := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal(description)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if proxy != "direct" {
		wait("proxy not ready", func() bool { result := fetch("/health"); return result.err == nil && result.body == "ready" })
	}
	if inherited && log.state("worker").spawned != 0 {
		t.Fatal("worker started before demand")
	}
	if !inherited {
		wait("minimum worker not ready", func() bool { return log.state("worker").telemetry == 1 })
	} else if proxy == "direct" {
		// Load completion is visible through the service log, without a probe connection.
		wait("listener not ready", func() bool {
			log.mu.Lock()
			defer log.mu.Unlock()
			return strings.Contains(strings.Join(log.lines, "\n"), "service configured")
		})
	}
	cold := check(fetch(prefix + "/0"))
	check(fetch(prefix + "/0"))
	replies := make(chan reply, 2)
	growthPath := "/600"
	if name == "SOCKETIFY" {
		growthPath = "/busy600"
	}
	for range 2 {
		go func() { replies <- fetch(prefix + growthPath) }()
	}
	wait("occupancy did not grow to two ready workers", func() bool { state := log.state("worker"); return state.live == 2 && state.telemetry == 2 })
	pids := map[string]bool{cold: true}
	// Connect while the original workload is still active. Waiting for its
	// responses first would erase the capacity shortage we mean to exercise.
	pids[check(fetch(prefix+"/0"))] = true
	for range 2 {
		pids[check(<-replies)] = true
	}
	for attempt := 0; len(pids) < 2 && attempt < 25; attempt++ {
		// Concurrent connections exercise capacity without assuming the kernel
		// distributes serial accepts fairly between ready processes.
		for range 4 {
			go func() { replies <- fetch(prefix + "/30") }()
		}
		for range 4 {
			pids[check(<-replies)] = true
		}
	}
	if len(pids) != 2 {
		t.Fatalf("new worker never served: %v", pids)
	}
	var next string
	if inherited {
		wait("idle workers did not return to zero", func() bool { return log.state("worker").live == 0 })
		next = check(fetch(prefix + "/0"))
		if pids[next] {
			t.Fatal("idle worker was not replaced")
		}
	} else {
		wait("idle workers did not return to minimum one", func() bool { return log.state("worker").live == 1 })
		next = check(fetch(prefix + "/0"))
		if !pids[next] {
			t.Fatal("minimum worker was unnecessarily replaced")
		}
		wait("minimum response telemetry not complete before crash", func() bool {
			state := log.state("worker")
			return state.starts == state.ends
		})
		// The minimum must restart a crashed ordinary process without socket demand.
		pid, _ := strconv.Atoi(strings.TrimPrefix(next, "worker="))
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		spawned := log.state("worker").spawned
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		process.Release()
		wait("minimum worker did not restart", func() bool {
			state := log.state("worker")
			return state.live == 1 && state.spawned > spawned && state.telemetry == state.spawned
		})
		if check(fetch(prefix+"/0")) == next {
			t.Fatal("crashed worker was not replaced")
		}
	}
	var websocket *testWebSocket
	if protocol == "http1" && name != "PYTHON" {
		peer := openTestWebSocket(t, requestAddress)
		peer.echo(t)
		time.Sleep(app.IdleTimeout + 100*time.Millisecond)
		peer.echo(t)
		peer.close(t, false)
		peer = openTestWebSocket(t, requestAddress)
		peer.echo(t)
		websocket = &peer
	}
	wait("reactivation telemetry not complete", func() bool {
		state := log.state("worker")
		active := 0
		if websocket != nil {
			active = 1
		}
		return state.starts-state.ends == active
	})
	starts := log.state("worker").starts
	go func() { replies <- fetch(prefix + "/600") }()
	wait("final request did not begin", func() bool { return log.state("worker").starts > starts })
	cancel()
	if websocket != nil {
		websocket.close(t, true)
		t.Log("WebSocket upgrade, text/binary echo, ping/pong, idle survival and bidirectional close handshake")
	}
	check(<-replies)
	if inherited {
		t.Log("cold/hot response, automatic growth, both PIDs, balanced events, idle zero, reactivation and active-request drain")
	} else {
		t.Log("worker-owned listeners: minimum one, growth, both PIDs, balanced events, idle one, crash restart and drain")
	}
}
