package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"
)

type proxyWorkerState struct {
	live, spawned, starts, ends, telemetry int
}

type proxyLog struct {
	mu    sync.Mutex
	apps  map[string]proxyWorkerState
	lines []string
}

func (*proxyLog) Enabled(context.Context, slog.Level) bool { return true }
func (log *proxyLog) WithAttrs([]slog.Attr) slog.Handler   { return log }
func (log *proxyLog) WithGroup(string) slog.Handler        { return log }
func (log *proxyLog) Handle(_ context.Context, record slog.Record) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	var app string
	var telemetry bool
	line := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		line += fmt.Sprintf(" %s=%v", attr.Key, attr.Value.Any())
		if attr.Key == "app" {
			app = attr.Value.String()
		}
		if attr.Key == "telemetry" {
			telemetry = attr.Value.Bool()
		}
		return true
	})
	log.lines = append(log.lines, line)
	state := log.apps[app]
	switch record.Message {
	case "worker started":
		state.live++
		state.spawned++
	case "worker exited":
		state.live--
	case "request started":
		state.starts++
	case "request finished":
		state.ends++
	case "worker stdout detected":
		if telemetry {
			state.telemetry++
		}
	}
	log.apps[app] = state
	return nil
}

func (log *proxyLog) state(app string) proxyWorkerState {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.apps[app]
}

func renderProxyFixture(t *testing.T, name, destination string, data any) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "test", "integration", "proxy", name))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := template.New(name).Parse(string(content))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	write(t, destination, output.String())
}

func TestProxyStack(t *testing.T) {
	runtimes := make(map[string]string)
	for _, name := range []string{"PYTHON", "NODE", "PHP_CGI"} {
		path := os.Getenv("OOTH_TEST_" + name)
		if path == "" {
			t.Skip("set OOTH_TEST_PYTHON, OOTH_TEST_NODE and OOTH_TEST_PHP_CGI for the full proxy suite")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		runtimes[name] = absolute
	}
	for _, proxy := range []string{"caddy", "nginx"} {
		t.Run(proxy, func(t *testing.T) {
			executable := os.Getenv("OOTH_TEST_" + strings.ToUpper(proxy))
			if executable == "" {
				t.Skip("set OOTH_TEST_" + strings.ToUpper(proxy))
			}
			executable, err := filepath.Abs(executable)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			if root := os.Getenv("OOTH_TEST_ARTIFACTS"); root != "" {
				if err := os.MkdirAll(root, 0755); err != nil {
					t.Fatal(err)
				}
				directory, err = os.MkdirTemp(root, proxy+"-")
				if err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.ToSlash(directory)
			addresses := map[string]string{"Root": root, "Address": freeAddress(t), "Python": freeAddress(t), "Node": freeAddress(t), "PHP": freeAddress(t)}
			renderProxyFixture(t, "ooth.yaml", filepath.Join(directory, "ooth.yaml"), nil)
			renderProxyFixture(t, "worker.php", filepath.Join(directory, "php", "worker.php"), nil)
			write(t, filepath.Join(directory, "health"), "ready")
			if err := os.MkdirAll(filepath.Join(directory, "logs"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(directory, "temp"), 0755); err != nil {
				t.Fatal(err)
			}
			python, _ := filepath.Abs("../examples/worker.py")
			node, _ := filepath.Abs("../test/integration/node_worker.cjs")
			phpCommand := []string{runtimes["PHP_CGI"], "-n", "-d", "cgi.force_redirect=0"}
			if launcher := os.Getenv("OOTH_TEST_PHP_LAUNCHER"); launcher != "" {
				absolute, err := filepath.Abs(launcher)
				if err != nil {
					t.Fatal(err)
				}
				phpCommand = append([]string{absolute}, phpCommand...)
			}
			for _, worker := range []struct {
				Name      string
				Command   []string
				Env       map[string]string
				Address   string
				Telemetry bool
			}{
				{"python", []string{runtimes["PYTHON"], python}, nil, addresses["Python"], true},
				{"node", []string{runtimes["NODE"], node}, map[string]string{"TEST_NODE_H2C": "1"}, addresses["Node"], true},
				{"php", phpCommand, map[string]string{"PHP_FCGI_MAX_REQUESTS": "0"}, addresses["PHP"], false},
			} {
				renderProxyFixture(t, "worker.yaml.tmpl", filepath.Join(directory, "apps", worker.Name+".yaml"), worker)
			}
			config := filepath.Join(directory, "Caddyfile")
			command := []string{executable, "run", "--config", config, "--adapter", "caddyfile"}
			if proxy == "nginx" {
				config = filepath.Join(directory, "nginx.conf")
				command = []string{executable, "-p", root + "/", "-c", config}
				renderProxyFixture(t, "nginx.conf.tmpl", config, addresses)
			} else {
				renderProxyFixture(t, "Caddyfile.tmpl", config, addresses)
			}
			renderProxyFixture(t, "webserver.yaml.tmpl", filepath.Join(directory, "apps", "proxy.yaml"), map[string]any{"Command": command, "Root": root})
			log := &proxyLog{apps: make(map[string]proxyWorkerState)}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- Run(ctx, filepath.Join(directory, "ooth.yaml"), slog.New(log)) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(8 * time.Second):
					t.Error("proxy stack shutdown timeout")
				}
				log.mu.Lock()
				defer log.mu.Unlock()
				text := strings.Join(log.lines, "\n")
				os.WriteFile(filepath.Join(directory, "ooth.log"), []byte(text+"\n"), 0600)
				if t.Failed() {
					t.Log(text)
				}
				for name, state := range log.apps {
					if state.live != 0 {
						t.Errorf("%s retained %d processes", name, state.live)
					}
				}
			}()
			client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			defer client.CloseIdleConnections()
			fetch := func(path string) (string, error) {
				response, err := client.Get("http://" + addresses["Address"] + path)
				if err != nil {
					return "", err
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if response.StatusCode != 200 {
					return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, body)
				}
				return strings.TrimSpace(string(body)), err
			}
			wait := func(description string, condition func() bool) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for !condition() {
					if time.Now().After(deadline) {
						t.Fatal(description)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			wait("webserver not started by ooth", func() bool { body, err := fetch("/health"); return err == nil && body == "ready" })
			for _, name := range []string{"python", "node", "php"} {
				if log.state(name).spawned != 0 {
					t.Fatalf("%s started before socket demand", name)
				}
			}
			for _, name := range []string{"python", "node"} {
				first, err := fetch("/" + name + "/0")
				if err != nil || !strings.HasPrefix(first, "worker=") {
					t.Fatalf("%s cold reply: %s %v", name, first, err)
				}
				if name == "node" && !strings.Contains(first, "protocol=h2c") {
					t.Fatal("Node was not reached through h2c")
				}
				type reply struct {
					body string
					err  error
				}
				replies := make(chan reply, 2)
				for range 2 {
					go func() { body, err := fetch("/" + name + "/600"); replies <- reply{body, err} }()
				}
				wait(name+" did not grow to two processes", func() bool { return log.state(name).live == 2 })
				pids := make(map[string]bool)
				for range 2 {
					result := <-replies
					if result.err != nil {
						t.Fatal(result.err)
					}
					pids[strings.Fields(result.body)[0]] = true
				}
				// Initial requests can already be accepted before the second
				// process starts. Only new connections can reach that process.
				for attempt := 0; name == "node" && len(pids) < 2 && attempt < 8; attempt++ {
					body, err := fetch("/node/0")
					if err != nil {
						t.Fatal(err)
					}
					pids[strings.Fields(body)[0]] = true
				}
				if len(pids) != 2 {
					t.Fatalf("%s requests did not reach both workers: %v", name, pids)
				}
				wait(name+" did not shrink to zero", func() bool { return log.state(name).live == 0 })
				state := log.state(name)
				if state.starts < 3 || state.starts != state.ends || state.telemetry != state.spawned {
					t.Fatalf("%s telemetry incomplete: %+v", name, state)
				}
				next, err := fetch("/" + name + "/0")
				if err != nil || pids[strings.Fields(next)[0]] {
					t.Fatalf("%s reactivation: %s %v", name, next, err)
				}
				t.Logf("%s: cold response, two worker PIDs, %d start/end pairs, idle zero, reactivation", name, state.ends)
			}
			first, err := fetch("/php/worker.php?delay=300")
			if err != nil || !strings.Contains(first, "protocol=fastcgi") {
				t.Fatalf("PHP cold response: %s %v", first, err)
			}
			time.Sleep(1100 * time.Millisecond)
			next, err := fetch("/php/worker.php?delay=300")
			state := log.state("php")
			if err != nil || first != next || state.live != 1 || state.spawned != 1 || state.telemetry != 0 || state.starts != 0 {
				t.Fatalf("PHP should remain an ordinary worker: first=%s next=%s state=%+v err=%v", first, next, state, err)
			}
			t.Log("PHP: cold FastCGI response, no handshake, same process beyond request/idle timeouts")
		})
	}
}
