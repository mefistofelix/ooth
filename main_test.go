package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestProtocol(t *testing.T) {
	for _, event := range []Event{{Type: "ready", Time: 1}, {Type: "start", Time: 2, ID: "abc"}, {Type: "end", Time: 3, ID: "abc", DurationNS: 7}} {
		var buffer bytes.Buffer
		if err := WriteEvent(&buffer, event); err != nil {
			t.Fatal(err)
		}
		got, err := ParseEvent(buffer.String())
		if err != nil || got != event {
			t.Fatalf("%+v %v", got, err)
		}
	}
	for _, line := range []string{"v=2 event=ready ts=1", "v=1 v=1 event=ready ts=1", "v=1 event=start ts=1", "v=1 event=end ts=1 id=a duration_ns=-1", "v=1 event=ready ts=1 garbage=yes"} {
		if _, err := ParseEvent(line); err == nil {
			t.Fatalf("accepted %q", line)
		}
	}
}

func TestConfig(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['app*/ooth.yaml']\n")
	write(t, filepath.Join(directory, "app1", "ooth.yaml"), "name: web\ncommand: ['./worker']\nlisten: {network: tcp, address: '127.0.0.1:12345'}\nrequires: [db]\n")
	write(t, filepath.Join(directory, "app2", "ooth.yaml"), "name: db\ncommand: ['db']\n")
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Apps["web"].Ready != "started" || config.Apps["db"].Ready != "started" {
		t.Fatal("readiness defaults")
	}
	if !filepath.IsAbs(config.Apps["web"].Command[0]) {
		t.Fatal("relative command not resolved")
	}
	write(t, filepath.Join(directory, "app2", "ooth.yaml"), "name: db\ncommand: ['db']\nrequires: [web]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted dependency cycle")
	}
	write(t, filepath.Join(directory, "app2", "ooth.yaml"), "name: db\ncommand: ['db']\nunknown: yes\n")
	if _, err := Load(path); err != nil {
		t.Fatal("unknown key should be ignored:", err)
	}
}

// The same test executable acts as an independent worker process. All payload
// traffic goes through the inherited listener; stdout only carries events.
func TestWorkerHelper(t *testing.T) {
	if os.Getenv("TEST_OOTH_WORKER") != "1" {
		return
	}
	stopSignal := os.Interrupt
	if runtime.GOOS == "linux" {
		stopSignal = syscall.SIGTERM
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, stopSignal)
	defer cancel()
	if os.Getenv("TEST_OOTH_IGNORE_STOP") == "1" {
		ctx = context.Background()
	} else if os.Getenv("OOTH_LISTEN_HANDLE") != "" || os.Getenv("TEST_OOTH_VIRTUAL") == "1" || os.Getenv("TEST_OOTH_BIND") != "" {
		testWorkerControl(cancel)
	}
	emit := func(event Event) {
		if os.Getenv("TEST_OOTH_STDOUT") == "silent" {
			return
		}
		event.Time = time.Now().UnixNano()
		if err := WriteEvent(os.Stdout, event); err != nil {
			os.Exit(2)
		}
	}
	if os.Getenv("TEST_OOTH_VIRTUAL") == "1" {
		if marker := os.Getenv("TEST_OOTH_MARKER"); marker != "" {
			os.WriteFile(marker, []byte("ready"), 0600)
		}
		emit(Event{Type: "ready"})
		<-ctx.Done()
		os.Exit(0)
	}
	var listener net.Listener
	var err error
	if address := os.Getenv("TEST_OOTH_BIND"); address != "" {
		listener, err = net.Listen("tcp", address)
	} else {
		listener, err = testWorkerListener()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var cached []byte
	if source := os.Getenv("TEST_OOTH_SOURCE"); source != "" {
		cached, err = os.ReadFile(source)
		if err != nil {
			os.Exit(2)
		}
	}
	go func() {
		<-ctx.Done()
		listener.Close()
		if directory := os.Getenv("TEST_OOTH_CLOSED"); directory != "" {
			os.WriteFile(filepath.Join(directory, strconv.Itoa(os.Getpid())), []byte("closed"), 0600)
		}
	}()
	switch os.Getenv("TEST_OOTH_STDOUT") {
	case "plain":
		fmt.Fprintln(os.Stdout, "ordinary startup log")
	case "long":
		fmt.Fprintln(os.Stdout, strings.Repeat("x", 9000))
	case "partial":
		fmt.Fprint(os.Stdout, "v=1 event=ready ts=1")
		os.Stdout.Close()
		emit = func(Event) {}
	}
	emit(Event{Type: "ready"})
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				os.Exit(0)
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		line, err := bufio.NewReader(connection).ReadString('\n')
		if err != nil {
			connection.Close()
			continue
		}
		fields := strings.Fields(line)
		delay, _ := time.ParseDuration(fields[0])
		id := fields[1]
		emit(Event{Type: "start", ID: id})
		time.Sleep(delay)
		fmt.Fprintf(connection, "%d %s%s\n", os.Getpid(), id, cached)
		connection.Close()
		emit(Event{Type: "end", ID: id, DurationNS: delay.Nanoseconds()})
		if ctx.Err() != nil {
			os.Exit(0)
		}
	}
}

func TestActivationLifecycle(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			directory := t.TempDir()
			address := filepath.Join(directory, "a.sock")
			if network == "tcp" {
				address = freeAddress(t)
			}
			executable, _ := os.Executable()
			app := App{Name: "web", Command: []string{executable, "-test.run=^TestWorkerHelper$"}, Env: map[string]string{"TEST_OOTH_WORKER": "1"}, Listen: Socket{network, address}, MaxWorkers: 2, Concurrency: 1, IdleTimeout: 250 * time.Millisecond, StartTimeout: 3 * time.Second, StopTimeout: time.Second, ScaleDelay: 50 * time.Millisecond}
			path := filepath.Join(directory, "ooth.yaml")
			write(t, path, "watch: ['app/ooth.yaml']\n"+unlimitedTestResources)
			appPath := filepath.Join(directory, "app", "ooth.yaml")
			writeApp(t, appPath, app)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			log := &proxyLog{apps: make(map[string]proxyWorkerState)}
			go func() { done <- Run(ctx, path, slog.New(log)) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("shutdown timeout")
				}
			})
			first := request(t, network, address, "10ms first")
			// Race-instrumented helpers can deliberately delay process exit.
			deadline := time.Now().Add(3 * time.Second)
			for log.state("web").live != 0 && time.Now().Before(deadline) {
				if log.state("web").spawned != 1 {
					t.Fatal("idle retirement started an unsolicited replacement")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if state := log.state("web"); state.live != 0 || state.spawned != 1 {
				t.Fatalf("idle pool must remain at zero: %+v", state)
			}
			second := request(t, network, address, "10ms second")
			if strings.Fields(first)[0] == strings.Fields(second)[0] {
				t.Fatal("idle worker was not replaced")
			}
			long := connect(t, network, address)
			fmt.Fprintln(long, "400ms long")
			time.Sleep(150 * time.Millisecond)
			short := request(t, network, address, "5ms short")
			longReply, err := bufio.NewReader(long).ReadString('\n')
			long.Close()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Fields(short)[0] == strings.Fields(longReply)[0] {
				t.Fatal("pool did not scale")
			}
			// An invalid atomic update must keep the current listener usable.
			write(t, appPath, "command: [bad]\nmax_workers: 0\n")
			time.Sleep(250 * time.Millisecond)
			request(t, network, address, "5ms after-rejected-config")
		})
	}
}

func TestVirtualDependency(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "dependency-ready")
	executable, _ := os.Executable()
	dependency := App{Name: "db", Command: []string{executable, "-test.run=^TestWorkerHelper$"}, Env: map[string]string{"TEST_OOTH_WORKER": "1", "TEST_OOTH_VIRTUAL": "1", "TEST_OOTH_MARKER": marker}, Ready: "event", MaxWorkers: 1, Concurrency: 1, IdleTimeout: time.Second, StartTimeout: 3 * time.Second, StopTimeout: time.Second, ScaleDelay: 25 * time.Millisecond}
	web := dependency
	web.Name = "web"
	web.Env = map[string]string{"TEST_OOTH_WORKER": "1"}
	web.Requires = []string{"db"}
	web.Listen = Socket{"tcp", freeAddress(t)}
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['*/app.yaml']\n")
	writeApp(t, filepath.Join(directory, "db", "app.yaml"), dependency)
	writeApp(t, filepath.Join(directory, "web", "app.yaml"), web)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("dependency started before demand")
	}
	request(t, "tcp", web.Listen.Address, "5ms dependency")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("dependency was not activated", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dependency shutdown timeout")
	}
}

func TestGracefulAndForceful(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(strconv.FormatBool(ignore), func(t *testing.T) {
			directory := t.TempDir()
			executable, _ := os.Executable()
			address := freeAddress(t)
			app := App{Name: "web", Command: []string{executable, "-test.run=^TestWorkerHelper$"}, Env: map[string]string{"TEST_OOTH_WORKER": "1"}, Listen: Socket{"tcp", address}, MaxWorkers: 1, Concurrency: 1, IdleTimeout: time.Second, StartTimeout: 3 * time.Second, StopTimeout: 400 * time.Millisecond, ScaleDelay: 25 * time.Millisecond}
			if ignore {
				app.Env["TEST_OOTH_IGNORE_STOP"] = "1"
			}
			path := filepath.Join(directory, "ooth.yaml")
			write(t, path, "watch: ['app/app.yaml']\n")
			writeApp(t, filepath.Join(directory, "app", "app.yaml"), app)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
			request(t, "tcp", address, "1ms warmup")
			connection := connect(t, "tcp", address)
			fmt.Fprintln(connection, "250ms draining")
			time.Sleep(75 * time.Millisecond)
			start := time.Now()
			cancel()
			response, err := bufio.NewReader(connection).ReadString('\n')
			connection.Close()
			if err != nil || !strings.Contains(response, "draining") {
				t.Fatalf("active request interrupted: %q %v", response, err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("forceful timeout")
			}
			if ignore && time.Since(start) < app.StopTimeout {
				t.Fatal("ignored signal exited before force timeout")
			}
		})
	}
}

func TestPythonWorker(t *testing.T) {
	python := os.Getenv("OOTH_TEST_PYTHON")
	if python == "" {
		t.Skip("set OOTH_TEST_PYTHON to test the external-language example")
	}
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			if runtime.GOOS == "windows" && network == "unix" {
				t.Skip("CPython's Windows socket module does not implement AF_UNIX accept")
			}
			directory := t.TempDir()
			worker, err := filepath.Abs("examples/worker.py")
			if err != nil {
				t.Fatal(err)
			}
			address := filepath.Join(directory, "python.sock")
			if network == "tcp" {
				address = freeAddress(t)
			}
			app := App{Name: "python", Command: []string{python, worker}, Listen: Socket{network, address}, MaxWorkers: 2, Concurrency: 1, IdleTimeout: 250 * time.Millisecond, StartTimeout: 3 * time.Second, StopTimeout: time.Second, ScaleDelay: 50 * time.Millisecond}
			path := filepath.Join(directory, "ooth.yaml")
			write(t, path, "watch: ['app/ooth.yaml']\n")
			writeApp(t, filepath.Join(directory, "app", "ooth.yaml"), app)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
			var previous string
			for range 2 {
				connection := connect(t, network, address)
				fmt.Fprint(connection, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
				response, err := http.ReadResponse(bufio.NewReader(connection), nil)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				connection.Close()
				if err != nil || !strings.HasPrefix(string(body), "worker=") || previous == string(body) {
					t.Fatalf("Python reply: %q %v", body, err)
				}
				previous = string(body)
				time.Sleep(800 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Python shutdown timeout")
			}
		})
	}
}

func TestWatchAddReloadRemove(t *testing.T) {
	for _, pattern := range []string{"app*/ooth.yaml", "app*/**/ooth.yaml"} {
		t.Run(pattern, func(t *testing.T) { testWatchAddReloadRemove(t, pattern) })
	}
}

func testWatchAddReloadRemove(t *testing.T, pattern string) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['"+pattern+"']\n")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	time.Sleep(100 * time.Millisecond)
	executable, _ := os.Executable()
	address := freeAddress(t)
	app := App{Name: "web", Command: []string{executable, "-test.run=^TestWorkerHelper$"}, Env: map[string]string{"TEST_OOTH_WORKER": "1"}, Listen: Socket{"tcp", address}, MaxWorkers: 1, Concurrency: 1, IdleTimeout: time.Minute, StartTimeout: 3 * time.Second, StopTimeout: time.Second, ScaleDelay: 25 * time.Millisecond}
	appPath := filepath.Join(directory, "app1", "ooth.yaml")
	if strings.Contains(pattern, "**") {
		appPath = filepath.Join(directory, "app1", "nested", "deep", "ooth.yaml")
	}
	writeApp(t, appPath, app)
	first := request(t, "tcp", address, "1ms added")
	app.Env["RELOAD_MARKER"] = "changed"
	writeApp(t, appPath, app)
	deadline := time.Now().Add(6 * time.Second)
	for {
		reply := request(t, "tcp", address, "1ms reloaded")
		if strings.Fields(reply)[0] != strings.Fields(first)[0] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("configuration update did not replace the worker")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := os.Remove(appPath); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(6 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			break
		}
		connection.Close()
		if time.Now().After(deadline) {
			t.Fatal("removed app is still listening")
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func writeApp(t *testing.T, path string, app App) {
	t.Helper()
	data, err := yaml.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data))
}
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}
func connect(t *testing.T, network, address string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		connection, err := net.DialTimeout(network, address, 100*time.Millisecond)
		if err == nil {
			connection.SetDeadline(time.Now().Add(4 * time.Second))
			return connection
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func request(t *testing.T, network, address, payload string) string {
	t.Helper()
	connection := connect(t, network, address)
	defer connection.Close()
	fmt.Fprintln(connection, payload)
	result, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return result
}
