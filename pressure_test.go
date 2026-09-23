package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in measurements, not performance assertions. Every worker is a separate
// process with the real inherited listener and the ordinary ready/start/end
// protocol. Accept timestamps travel in test responses, not in that protocol.
func TestPressureWorker(t *testing.T) {
	mode := os.Getenv("TEST_PRESSURE_WORKER")
	if mode == "" {
		return
	}
	listener, err := net.FileListener(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Stdin.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, StopSignal)
	defer stop()
	go func() { <-ctx.Done(); listener.Close() }()
	var output sync.Mutex
	emit := func(kind, id string, duration int64) {
		output.Lock()
		defer output.Unlock()
		if err := WriteEvent(os.Stdout, Event{Type: kind, ID: id, Time: time.Now().UnixNano(), DurationNS: duration}); err != nil {
			os.Exit(2)
		}
	}
	emit("ready", "", 0)
	var requests sync.WaitGroup
	slots := make(chan struct{}, 1)
	handle := func(connection net.Conn, accepted int64) {
		defer requests.Done()
		defer connection.Close()
		reader := bufio.NewReader(connection)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) != 2 {
				os.Exit(2)
			}
			delay, err := time.ParseDuration(fields[1])
			if err != nil {
				os.Exit(2)
			}
			// greedy accepts freely but has only one application execution slot.
			if mode == "greedy" {
				slots <- struct{}{}
			}
			started := time.Now()
			emit("start", fields[0], 0)
			time.Sleep(delay)
			emit("end", fields[0], time.Since(started).Nanoseconds())
			if mode == "greedy" {
				<-slots
			}
			if _, err := fmt.Fprintf(connection, "%s %d %d %d\n", fields[0], os.Getpid(), accepted, started.UnixNano()); err != nil {
				return
			}
		}
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			requests.Wait()
			os.Exit(0)
		}
		accepted := time.Now().UnixNano()
		requests.Add(1)
		if mode == "serial" {
			handle(connection, accepted)
		} else {
			go handle(connection, accepted)
		}
	}
}

type pressureEvent struct {
	Time int64
	Kind string
	PID  int64
	ID   string
}

type pressureLog struct {
	mu      sync.Mutex
	events  []pressureEvent
	ready   int
	spawned int
}

func (*pressureLog) Enabled(context.Context, slog.Level) bool { return true }
func (log *pressureLog) WithAttrs([]slog.Attr) slog.Handler   { return log }
func (log *pressureLog) WithGroup(string) slog.Handler        { return log }
func (log *pressureLog) Handle(_ context.Context, record slog.Record) error {
	event := pressureEvent{Time: record.Time.UnixNano(), Kind: record.Message}
	telemetry := false
	app := ""
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "app":
			app = attr.Value.String()
		case "pid":
			event.PID = attr.Value.Int64()
		case "id":
			event.ID = attr.Value.String()
		case "telemetry":
			telemetry = attr.Value.Bool()
		case "observed_ns":
			event.Time = attr.Value.Int64()
		}
		return true
	})
	if app != "pressure" {
		return nil
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, event)
	if record.Message == "worker started" {
		log.spawned++
	}
	if telemetry {
		log.ready++
	}
	return nil
}

type pressureCase struct {
	Name, Mode, Runtime, Proxy string
	Min, Max, Capacity         int
	Rate, Count                int
	Delay                      time.Duration
	Burst, Persistent          bool
	Jitter                     bool
	Block                      bool
}

type pressureRequest struct {
	ID                                                        int
	Scheduled, Dispatched, Connected, Accepted, Started, Done int64
	PID                                                       int
	Error                                                     string
	Delay                                                     time.Duration
}

func pressureReply(request *pressureRequest, reader *bufio.Reader) {
	line, err := reader.ReadString('\n')
	request.Done = time.Now().UnixNano()
	if err != nil {
		request.Error = err.Error()
		return
	}
	var id int
	_, err = fmt.Sscanf(line, "%d %d %d %d", &id, &request.PID, &request.Accepted, &request.Started)
	if err != nil || id != request.ID {
		request.Error = fmt.Sprintf("invalid response %q: %v", line, err)
	}
}

func TestPressure(t *testing.T) {
	root := os.Getenv("OOTH_TEST_PRESSURE")
	if root == "" {
		t.Skip("set OOTH_TEST_PRESSURE to a results directory for the open-loop load matrix")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	cases := []pressureCase{
		{Name: "serial-low-1", Mode: "serial", Min: 1, Max: 1, Rate: 10},
		{Name: "serial-load-1", Mode: "serial", Min: 1, Max: 1, Rate: 70},
		{Name: "serial-load-2", Mode: "serial", Min: 2, Max: 2, Rate: 70},
		{Name: "serial-load-4", Mode: "serial", Min: 4, Max: 4, Rate: 70},
		{Name: "serial-fast-1", Mode: "serial", Min: 1, Max: 1, Rate: 200, Delay: time.Millisecond},
		{Name: "serial-burst-1", Mode: "serial", Min: 1, Max: 1, Burst: true, Count: 80},
		{Name: "serial-burst-4", Mode: "serial", Min: 4, Max: 4, Burst: true, Count: 80},
		{Name: "greedy-load-1", Mode: "greedy", Min: 1, Max: 1, Rate: 70},
		{Name: "parallel-load-1", Mode: "parallel", Min: 1, Max: 1, Rate: 70},
		{Name: "persistent-load-1", Mode: "serial", Min: 1, Max: 1, Rate: 70, Persistent: true},
		{Name: "adaptive-low", Mode: "serial", Min: 1, Max: 4, Rate: 10},
		{Name: "adaptive-serial", Mode: "serial", Min: 1, Max: 4, Rate: 70},
		{Name: "adaptive-parallel-c1", Mode: "parallel", Min: 1, Max: 4, Rate: 70},
		{Name: "adaptive-parallel-c4", Mode: "parallel", Min: 1, Max: 4, Rate: 70, Capacity: 4},
		{Name: "adaptive-persistent", Mode: "serial", Min: 1, Max: 4, Rate: 70, Persistent: true},
	}
	for _, scenario := range cases {
		t.Run(scenario.Name, func(t *testing.T) { runPressure(t, root, scenario) })
	}
	for _, runtime := range []string{"PYTHON", "NODE", "PHP_CGI"} {
		t.Run(strings.ToLower(runtime), func(t *testing.T) {
			if os.Getenv("OOTH_TEST_"+runtime) == "" {
				t.Skip("set OOTH_TEST_" + runtime)
			}
			for _, workers := range []int{1, 2, 4} {
				for _, rate := range []int{10, 70} {
					scenario := pressureCase{Name: fmt.Sprintf("%s-r%d-w%d", strings.ToLower(runtime), rate, workers),
						Runtime: runtime, Mode: "serial", Min: workers, Max: workers, Rate: rate, Jitter: true}
					t.Run(scenario.Name, func(t *testing.T) { runPressure(t, root, scenario) })
				}
			}
		})
	}
}

func TestPressureProxies(t *testing.T) {
	root := os.Getenv("OOTH_TEST_PRESSURE")
	if root == "" {
		t.Skip("set OOTH_TEST_PRESSURE and runtime paths")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, proxy := range []string{"caddy", "nginx"} {
		t.Run(proxy, func(t *testing.T) {
			if os.Getenv("OOTH_TEST_"+strings.ToUpper(proxy)) == "" {
				t.Skip("set proxy runtime path")
			}
			for _, runtime := range []string{"PYTHON", "NODE", "PHP_CGI"} {
				if os.Getenv("OOTH_TEST_"+runtime) == "" {
					t.Fatal("proxy matrix requires PYTHON, NODE and PHP_CGI")
				}
				for _, workers := range []int{1, 2, 4} {
					scenario := pressureCase{Name: fmt.Sprintf("%s-%s-w%d", proxy, strings.ToLower(runtime), workers),
						Runtime: runtime, Proxy: proxy, Min: workers, Max: workers, Rate: 70, Jitter: true}
					t.Run(scenario.Name, func(t *testing.T) { runPressure(t, root, scenario) })
				}
				scenario := pressureCase{Name: proxy + "-" + strings.ToLower(runtime) + "-low", Runtime: runtime,
					Proxy: proxy, Min: 1, Max: 1, Rate: 10, Jitter: true}
				t.Run(scenario.Name, func(t *testing.T) { runPressure(t, root, scenario) })
			}
			for _, workers := range []int{1, 4} {
				scenario := pressureCase{Name: fmt.Sprintf("%s-node-block-w%d", proxy, workers), Runtime: "NODE", Proxy: proxy,
					Min: workers, Max: workers, Rate: 70, Jitter: true, Block: true}
				t.Run(scenario.Name, func(t *testing.T) { runPressure(t, root, scenario) })
			}
		})
	}
}

func runPressure(t *testing.T, root string, scenario pressureCase) {
	if scenario.Delay == 0 {
		scenario.Delay = 40 * time.Millisecond
	}
	if scenario.Capacity == 0 {
		scenario.Capacity = 1
	}
	if scenario.Count == 0 {
		scenario.Count = scenario.Rate * 2
	}
	directory, err := os.MkdirTemp(root, scenario.Name+"-")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	address := freeAddress(t)
	app := App{Name: "pressure", Command: []string{executable, "-test.run=^TestPressureWorker$"},
		Env: map[string]string{"TEST_PRESSURE_WORKER": scenario.Mode}, Listen: Socket{"tcp", address}, Ready: "event",
		MinWorkers: scenario.Min, MaxWorkers: scenario.Max, Concurrency: scenario.Capacity,
		ScaleDelay: 100 * time.Millisecond, IdleTimeout: time.Minute, StartTimeout: 5 * time.Second, StopTimeout: 3 * time.Second}
	if scenario.Runtime != "" {
		runtime, err := filepath.Abs(os.Getenv("OOTH_TEST_" + scenario.Runtime))
		if err != nil {
			t.Fatal(err)
		}
		switch scenario.Runtime {
		case "PYTHON", "NODE":
			extension := "py"
			if scenario.Runtime == "NODE" {
				extension = "cjs"
			}
			script, _ := filepath.Abs(filepath.Join("tools", "probes", "pressure", "worker."+extension))
			app.Command = []string{runtime, script}
			if scenario.Runtime == "NODE" && scenario.Proxy != "" {
				app.Env["TEST_PRESSURE_H2C"] = "1"
			}
			if scenario.Block {
				app.Env["TEST_PRESSURE_BLOCK"] = "1"
			}
		case "PHP_CGI":
			app.Command = []string{runtime, "-n", "-d", "cgi.force_redirect=0"}
			if launcher := os.Getenv("OOTH_TEST_PHP_LAUNCHER"); launcher != "" {
				launcher, _ = filepath.Abs(launcher)
				app.Command = append([]string{launcher}, app.Command...)
			}
			app.Env = map[string]string{"PHP_FCGI_MAX_REQUESTS": "0"}
			app.Ready = "started"
		}
	}
	writeApp(t, filepath.Join(directory, "apps", "worker.yaml"), app)
	clientAddress := address
	if scenario.Proxy != "" {
		clientAddress = pressureProxy(t, directory, address, scenario.Proxy)
	}
	config := filepath.Join(directory, "ooth.yaml")
	write(t, config, "watch: ['apps/*.yaml']\n")
	log := &pressureLog{}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- Run(ctx, config, slog.New(log)) }()
	defer func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("supervisor shutdown timed out")
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		rows := [][]string{{"time_ns", "event", "pid", "id"}}
		for _, event := range log.events {
			rows = append(rows, []string{strconv.FormatInt(event.Time, 10), event.Kind, strconv.FormatInt(event.PID, 10), event.ID})
		}
		pressureCSV(t, filepath.Join(directory, "events.csv"), rows)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for {
		log.mu.Lock()
		ready := log.ready
		if scenario.Runtime == "PHP_CGI" {
			ready = log.spawned
		}
		log.mu.Unlock()
		if ready >= scenario.Min {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workers did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if scenario.Runtime == "PHP_CGI" {
		time.Sleep(250 * time.Millisecond)
	}
	requests := make([]pressureRequest, scenario.Count)
	if scenario.Proxy != "" {
		client := &http.Client{Timeout: time.Second}
		defer client.CloseIdleConnections()
		for {
			response, err := client.Get("http://" + clientAddress + "/health")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("proxy not ready")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var clients sync.WaitGroup
	var persistent net.Conn
	var connected int64
	pending := make(chan *pressureRequest, scenario.Count)
	if scenario.Persistent {
		persistent, err = net.DialTimeout("tcp", address, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer persistent.Close()
		connected = time.Now().UnixNano()
		persistent.SetDeadline(time.Now().Add(15 * time.Second))
		clients.Add(1)
		go func() {
			defer clients.Done()
			reader := bufio.NewReader(persistent)
			for request := range pending {
				pressureReply(request, reader)
			}
		}()
	}
	// Do not wait for a response before scheduling the next request. Persistent
	// cases pipeline the same request stream over one already-accepted socket.
	start := time.Now().Add(100 * time.Millisecond)
	for id := range requests {
		request := &requests[id]
		request.ID = id
		request.Delay = scenario.Delay
		if scenario.Jitter {
			request.Delay = time.Duration(5+(id*37+17)%71) * time.Millisecond
		}
		scheduled := start
		if !scenario.Burst {
			scheduled = start.Add(time.Duration(id) * time.Second / time.Duration(scenario.Rate))
		}
		request.Scheduled = scheduled.UnixNano()
		time.Sleep(time.Until(scheduled))
		if persistent != nil {
			request.Dispatched = time.Now().UnixNano()
			request.Connected = connected
			if _, err := fmt.Fprintf(persistent, "%d %s\n", id, request.Delay); err != nil {
				request.Error = err.Error()
			}
			pending <- request
			continue
		}
		clients.Add(1)
		go func() {
			defer clients.Done()
			request.Dispatched = time.Now().UnixNano()
			connection, err := net.DialTimeout("tcp", clientAddress, 10*time.Second)
			request.Connected = time.Now().UnixNano()
			if err != nil {
				request.Error = err.Error()
				return
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(10 * time.Second))
			if scenario.Proxy != "" || scenario.Runtime == "PYTHON" || scenario.Runtime == "NODE" {
				pressureHTTP(request, connection, scenario)
				return
			}
			if scenario.Runtime == "PHP_CGI" {
				pressureFastCGI(request, connection)
				return
			}
			if _, err := fmt.Fprintf(connection, "%d %s\n", id, request.Delay); err != nil {
				request.Error = err.Error()
				return
			}
			pressureReply(request, bufio.NewReader(connection))
		}()
	}
	close(pending)
	clients.Wait()
	if persistent != nil {
		persistent.Close()
	}
	// Allow final pipe events to reach the manager before ending the sample.
	time.Sleep(100 * time.Millisecond)
	rows := [][]string{{"id", "scheduled_ns", "dispatched_ns", "connected_ns", "accepted_ns", "started_ns", "done_ns", "pid", "error", "delay_ms"}}
	for _, request := range requests {
		row := []string{strconv.Itoa(request.ID)}
		for _, timestamp := range []int64{request.Scheduled, request.Dispatched, request.Connected, request.Accepted, request.Started, request.Done} {
			row = append(row, strconv.FormatInt(timestamp, 10))
		}
		rows = append(rows, append(row, strconv.Itoa(request.PID), request.Error, strconv.FormatInt(request.Delay.Milliseconds(), 10)))
		if request.Error != "" {
			t.Errorf("request %d: %s", request.ID, request.Error)
		}
	}
	pressureCSV(t, filepath.Join(directory, "requests.csv"), rows)
	pressureCSV(t, filepath.Join(directory, "scenario.csv"), [][]string{
		{"name", "mode", "min", "max", "concurrency", "rate", "count", "delay_ms", "burst", "persistent", "start_ns", "load_end_ns", "runtime", "proxy"},
		{scenario.Name, scenario.Mode, strconv.Itoa(scenario.Min), strconv.Itoa(scenario.Max), strconv.Itoa(scenario.Capacity),
			strconv.Itoa(scenario.Rate), strconv.Itoa(scenario.Count), strconv.FormatInt(scenario.Delay.Milliseconds(), 10),
			strconv.FormatBool(scenario.Burst), strconv.FormatBool(scenario.Persistent), strconv.FormatInt(start.UnixNano(), 10),
			strconv.FormatInt(requests[len(requests)-1].Scheduled+int64(time.Second/time.Duration(max(scenario.Rate, 1))), 10), scenario.Runtime, scenario.Proxy},
	})
	t.Log(directory)
}

func pressureHTTP(request *pressureRequest, connection net.Conn, scenario pressureCase) {
	path := "/"
	if scenario.Proxy != "" {
		path += strings.ToLower(scenario.Runtime) + "/"
		if scenario.Runtime == "PHP_CGI" {
			path = "/php/worker.php"
		}
	}
	_, err := fmt.Fprintf(connection, "GET %s?id=%d&delay=%d HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, request.ID, request.Delay.Milliseconds(), connection.RemoteAddr())
	if err != nil {
		request.Error = err.Error()
		return
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		request.Error = err.Error()
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		body, _ := io.ReadAll(response.Body)
		request.Error = fmt.Sprintf("HTTP %d: %s", response.StatusCode, body)
		return
	}
	pressureReply(request, bufio.NewReader(response.Body))
}

func pressureProxy(t *testing.T, directory, backend, proxy string) string {
	t.Helper()
	root := filepath.ToSlash(directory)
	address := freeAddress(t)
	values := map[string]string{"Root": root, "Address": address, "Python": backend, "Node": backend, "PHP": backend}
	script, err := os.ReadFile(filepath.Join("tools", "probes", "pressure", "worker.php"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(directory, "php", "worker.php"), string(script))
	write(t, filepath.Join(directory, "health"), "ready")
	for _, name := range []string{"logs", "temp"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := filepath.Abs(os.Getenv("OOTH_TEST_" + strings.ToUpper(proxy)))
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "Caddyfile")
	command := []string{executable, "run", "--config", config, "--adapter", "caddyfile"}
	if proxy == "nginx" {
		config = filepath.Join(directory, "nginx.conf")
		command = []string{executable, "-p", root + "/", "-c", config}
		renderProxyFixture(t, "nginx.conf.tmpl", config, values)
		content, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		write(t, config, strings.ReplaceAll(string(content), "worker_connections 128", "worker_connections 1024"))
	} else {
		renderProxyFixture(t, "Caddyfile.tmpl", config, values)
	}
	renderProxyFixture(t, "webserver.yaml.tmpl", filepath.Join(directory, "apps", "proxy.yaml"), map[string]any{"Command": command, "Root": root})
	return address
}

func pressureCSV(t *testing.T, path string, rows [][]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	writer.WriteAll(rows)
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
}

// One stock FastCGI request per TCP connection; no third-party client package.
func pressureFastCGI(request *pressureRequest, connection net.Conn) {
	var wire bytes.Buffer
	record := func(kind byte, body []byte) {
		wire.Write([]byte{1, kind, 0, 1, byte(len(body) >> 8), byte(len(body)), 0, 0})
		wire.Write(body)
	}
	record(1, []byte{0, 1, 0, 0, 0, 0, 0, 0}) // responder, close after response
	script, _ := filepath.Abs(filepath.Join("tools", "probes", "pressure", "worker.php"))
	var params bytes.Buffer
	for key, value := range map[string]string{"SCRIPT_FILENAME": filepath.ToSlash(script), "REQUEST_METHOD": "GET",
		"QUERY_STRING": fmt.Sprintf("id=%d&delay=%d", request.ID, request.Delay.Milliseconds()), "SERVER_PROTOCOL": "HTTP/1.1", "GATEWAY_INTERFACE": "CGI/1.1"} {
		for _, length := range []int{len(key), len(value)} {
			if length < 128 {
				params.WriteByte(byte(length))
			} else {
				binary.Write(&params, binary.BigEndian, uint32(length)|0x80000000)
			}
		}
		params.WriteString(key)
		params.WriteString(value)
	}
	record(4, params.Bytes())
	record(4, nil)
	record(5, nil)
	if _, err := wire.WriteTo(connection); err != nil {
		request.Error = err.Error()
		return
	}
	var stdout, stderr bytes.Buffer
	for {
		var header [8]byte
		if _, err := io.ReadFull(connection, header[:]); err != nil {
			request.Error = err.Error()
			return
		}
		body := make([]byte, int(binary.BigEndian.Uint16(header[4:6]))+int(header[6]))
		if _, err := io.ReadFull(connection, body); err != nil {
			request.Error = err.Error()
			return
		}
		body = body[:len(body)-int(header[6])]
		switch header[1] {
		case 6:
			stdout.Write(body)
		case 7:
			stderr.Write(body)
		case 3:
			_, content, ok := strings.Cut(stdout.String(), "\r\n\r\n")
			if !ok {
				request.Error = "invalid CGI response: " + stdout.String() + stderr.String()
				return
			}
			pressureReply(request, bufio.NewReader(strings.NewReader(content)))
			return
		}
	}
}
