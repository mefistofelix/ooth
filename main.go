package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/sgtdi/fswatcher"
)

var version = "dev"
var StopSignal os.Signal = os.Interrupt

func main() {
	if runtime.GOOS == "linux" {
		StopSignal = syscall.SIGTERM
	}
	path := flag.String("config", "ooth.yaml", "main YAML configuration")
	check := flag.Bool("check", false, "validate configuration and exit")
	debug := flag.Bool("debug", false, "log request metrics")
	showVersion := flag.Bool("version", false, "print build version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	if *check {
		if _, err := Load(*path); err != nil {
			logger.Error("invalid configuration", "error", err)
			os.Exit(1)
		}
		fmt.Println("configuration valid")
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, StopSignal)
	defer cancel()
	if err := Run(ctx, *path, logger); err != nil {
		logger.Error("ooth stopped", "error", err)
		os.Exit(1)
	}
}

type Config struct {
	Watch  []string `yaml:"watch"`
	Cgroup string   `yaml:"cgroup"`
}

type Socket struct {
	Network string `yaml:"network"`
	Address string `yaml:"address"`
}

type Identity struct {
	User     string  `yaml:"user"`
	Group    string  `yaml:"group"`
	Password *string `yaml:"password"`
}

type App struct {
	Identity       Identity          `yaml:",inline"`
	Name           string            `yaml:"name"`
	Requires       []string          `yaml:"requires"`
	Startup        bool              `yaml:"startup"`
	Ready          string            `yaml:"ready"`
	Command        []string          `yaml:"command"`
	Directory      string            `yaml:"directory"`
	Env            map[string]string `yaml:"env"`
	Listen         Socket            `yaml:"listen"`
	MinWorkers     int               `yaml:"min_workers"`
	MaxWorkers     int               `yaml:"max_workers"`
	Concurrency    int               `yaml:"concurrency"`
	IdleTimeout    time.Duration     `yaml:"idle_timeout"`
	StartTimeout   time.Duration     `yaml:"start_timeout"`
	StopTimeout    time.Duration     `yaml:"stop_timeout"`
	RequestTimeout time.Duration     `yaml:"request_timeout"`
	ScaleDelay     time.Duration     `yaml:"scale_delay"`
	Source         string            `yaml:"-"`
}

type Snapshot struct {
	Apps   map[string]App
	Roots  []string
	Cgroup string
}

// Load validates the whole snapshot before it can replace running services.
// Relative paths resolve against the YAML file that contains them.
func Load(path string) (Snapshot, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Snapshot{}, err
	}
	var main Config
	if err := read(path, &main); err != nil {
		return Snapshot{}, err
	}
	if len(main.Watch) == 0 {
		return Snapshot{}, fmt.Errorf("%s: watch must contain at least one glob", path)
	}
	result := Snapshot{Apps: make(map[string]App), Roots: []string{filepath.Dir(path)}}
	if main.Cgroup != "" {
		result.Cgroup = resolve(filepath.Dir(path), main.Cgroup)
	}
	sockets := make(map[Socket]string)
	for _, pattern := range main.Watch {
		pattern = resolve(filepath.Dir(path), pattern)
		if strings.Contains(pattern, "**") {
			return Snapshot{}, fmt.Errorf("glob %q: use * for one directory level; ** is not supported", pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return Snapshot{}, fmt.Errorf("glob %q: %w", pattern, err)
		}
		root := pattern
		if index := strings.IndexAny(root, "*?["); index >= 0 {
			root = root[:index]
		}
		root = filepath.Dir(root)
		for {
			info, err := os.Stat(root)
			if err == nil && info.IsDir() {
				break
			}
			if err != nil && !os.IsNotExist(err) {
				return Snapshot{}, err
			}
			parent := filepath.Dir(root)
			if parent == root {
				return Snapshot{}, fmt.Errorf("cannot watch %q", pattern)
			}
			root = parent
		}
		result.Roots = append(result.Roots, root)
		for _, file := range matches {
			app := App{
				MaxWorkers: 1, Concurrency: 1, IdleTimeout: time.Minute,
				StartTimeout: 10 * time.Second, StopTimeout: 10 * time.Second,
				ScaleDelay: 100 * time.Millisecond,
			}
			if err := read(file, &app); err != nil {
				return Snapshot{}, err
			}
			app.Source = file
			if app.Name == "" {
				app.Name = filepath.Base(filepath.Dir(file))
			}
			if previous, ok := result.Apps[app.Name]; ok {
				if previous.Source == file {
					continue
				}
				return Snapshot{}, fmt.Errorf("duplicate app name %q", app.Name)
			}
			if app.Ready == "" {
				app.Ready = "started"
				if app.Listen.Network != "" {
					app.Ready = "event"
				}
			}
			if err := app.validate(); err != nil {
				return Snapshot{}, fmt.Errorf("%s: %w", file, err)
			}
			app.Directory = resolve(filepath.Dir(file), app.Directory)
			if strings.ContainsAny(app.Command[0], `/\`) {
				app.Command[0] = resolve(app.Directory, app.Command[0])
			}
			if app.Listen.Network == "unix" {
				app.Listen.Address = resolve(filepath.Dir(file), app.Listen.Address)
			}
			if app.Listen.Network != "" {
				if previous, ok := sockets[app.Listen]; ok {
					return Snapshot{}, fmt.Errorf("%s and %s use the same listener", previous, file)
				}
				sockets[app.Listen] = file
			}
			result.Apps[app.Name] = app
		}
	}
	slices.Sort(result.Roots)
	result.Roots = slices.Compact(result.Roots)
	var roots []string
	for _, root := range result.Roots {
		covered := false
		for _, parent := range roots {
			relative, err := filepath.Rel(parent, root)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			roots = append(roots, root)
		}
	}
	result.Roots = roots
	return result, validateDependencies(result.Apps)
}

func validateDependencies(apps map[string]App) error {
	state := make(map[string]int)
	var visit func(string) error
	visit = func(name string) error {
		app, exists := apps[name]
		if !exists {
			return fmt.Errorf("unknown dependency %q", name)
		}
		if state[name] == 1 {
			return fmt.Errorf("dependency cycle at %q", name)
		}
		if state[name] == 2 {
			return nil
		}
		state[name] = 1
		for _, dependency := range app.Requires {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for name := range apps {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func (app App) validate() error {
	if app.Identity.Password != nil && app.Identity.User == "" {
		return fmt.Errorf("password requires user")
	}
	if runtime.GOOS == "linux" && app.Identity.Password != nil {
		return fmt.Errorf("password is only supported on Windows")
	}
	if runtime.GOOS == "windows" && app.Identity.Group != "" {
		return fmt.Errorf("group is only supported on Linux")
	}
	if len(app.Command) == 0 || app.Command[0] == "" {
		return fmt.Errorf("command must be a non-empty argument list")
	}
	if app.Listen.Network != "" && app.Listen.Network != "tcp" && app.Listen.Network != "tcp4" && app.Listen.Network != "tcp6" && app.Listen.Network != "unix" {
		return fmt.Errorf("listen.network must be tcp, tcp4, tcp6 or unix")
	}
	if (app.Listen.Network == "") != (app.Listen.Address == "") {
		return fmt.Errorf("listen.address is required")
	}
	if app.Listen.Network == "" && app.MaxWorkers != 1 {
		return fmt.Errorf("virtual services have max_workers=1")
	}
	if app.Ready != "event" && app.Ready != "started" {
		network, address, ok := strings.Cut(app.Ready, "://")
		if !ok || address == "" || (network != "tcp" && network != "unix") {
			return fmt.Errorf("ready must be event, started, tcp://address or unix://path")
		}
	}
	if app.MinWorkers < 0 || app.MaxWorkers < 1 || app.MinWorkers > app.MaxWorkers || app.Concurrency < 1 {
		return fmt.Errorf("require 0 <= min_workers <= max_workers and positive max_workers/concurrency")
	}
	if app.IdleTimeout <= 0 || app.StartTimeout <= 0 || app.StopTimeout <= 0 || app.ScaleDelay <= 0 || app.RequestTimeout < 0 {
		return fmt.Errorf("timeouts and scale_delay must be positive; request_timeout may be zero")
	}
	for key, value := range app.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) || strings.HasPrefix(key, "OOTH_") {
			return fmt.Errorf("invalid or reserved environment variable %q", key)
		}
	}
	return nil
}

func read(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file, yaml.Strict())
	if err := decoder.Decode(target); err != nil {
		// Configuration can contain passwords; do not include YAML source excerpts.
		return fmt.Errorf("%s: %s", path, yaml.FormatError(err, false, false))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: expected exactly one YAML document", path)
	}
	return nil
}

func resolve(directory, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(directory, path)
}

// Watch rescans globs after filesystem events, including directory creation
// and atomic file replacement. Periodic reconciliation covers dropped events.
func Watch(ctx context.Context, path string, initial Snapshot, updates chan<- Snapshot) error {
	watcher, done, err := startWatcher(ctx, initial.Roots)
	if err != nil {
		return err
	}
	defer func() { watcher.Close(); <-done }()
	periodic := time.NewTicker(5 * time.Second)
	defer periodic.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case _, ok := <-watcher.Events():
			if !ok {
				return <-done
			}
			debounce.Reset(100 * time.Millisecond)
			continue
		case <-watcher.Dropped():
			debounce.Reset(100 * time.Millisecond)
			continue
		case <-debounce.C:
		case <-periodic.C:
		}
		next, err := Load(path)
		if err != nil {
			slog.Error("configuration rejected; keeping current services", "error", err)
			continue
		}
		if reflect.DeepEqual(initial, next) {
			continue
		}
		if !slices.Equal(initial.Roots, next.Roots) {
			replacement, replacementDone, err := startWatcher(ctx, next.Roots)
			if err != nil {
				slog.Error("watch update rejected", "error", err)
				continue
			}
			watcher.Close()
			<-done
			watcher, done = replacement, replacementDone
		}
		select {
		case updates <- next:
			initial = next
		case <-ctx.Done():
			return nil
		}
	}
}

func startWatcher(ctx context.Context, roots []string) (fswatcher.Watcher, chan error, error) {
	ready := make(chan struct{})
	options := []fswatcher.WatcherOpt{fswatcher.WithReadyChannel(ready)}
	for _, root := range roots {
		options = append(options, fswatcher.WithPath(root))
	}
	watcher, err := fswatcher.New(options...)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { defer close(done); done <- watcher.Watch(ctx) }()
	select {
	case <-ready:
		return watcher, done, nil
	case err := <-done:
		watcher.Close()
		if err == nil {
			err = fmt.Errorf("configuration watcher stopped before becoming ready")
		}
		return nil, nil, err
	case <-ctx.Done():
		watcher.Close()
		<-done
		return nil, nil, ctx.Err()
	}
}

const Version = 1

// Each stream contains space-separated key=value pairs, one event per line.
// Values cannot contain whitespace or '='. IDs are local to a worker.
type Event struct {
	Type       string
	Time       int64
	ID         string
	DurationNS int64
}

func WriteEvent(writer io.Writer, event Event) error {
	line := fmt.Sprintf("v=1 event=%s ts=%d", event.Type, event.Time)
	if event.Type == "start" || event.Type == "end" {
		if !validValue(event.ID) {
			return fmt.Errorf("invalid request id %q", event.ID)
		}
		line += " id=" + event.ID
	}
	if event.Type == "end" {
		line += " duration_ns=" + strconv.FormatInt(event.DurationNS, 10)
	}
	_, err := io.WriteString(writer, line+"\n")
	return err
}

func ParseEvent(line string) (Event, error) {
	fields, err := parseFields(line)
	if err != nil {
		return Event{}, err
	}
	event := Event{Type: fields["event"], ID: fields["id"]}
	event.Time, err = strconv.ParseInt(fields["ts"], 10, 64)
	if err != nil || event.Time <= 0 {
		return Event{}, fmt.Errorf("invalid event timestamp")
	}
	switch event.Type {
	case "ready":
		if len(fields) != 3 {
			return Event{}, fmt.Errorf("ready requires v, event, ts")
		}
	case "start", "end":
		if !validValue(event.ID) {
			return Event{}, fmt.Errorf("invalid request id")
		}
		count := 4
		if event.Type == "end" {
			count = 5
			event.DurationNS, err = strconv.ParseInt(fields["duration_ns"], 10, 64)
			if err != nil || event.DurationNS < 0 {
				return Event{}, fmt.Errorf("invalid request duration")
			}
		}
		if len(fields) != count {
			return Event{}, fmt.Errorf("unexpected fields in %s", event.Type)
		}
	default:
		return Event{}, fmt.Errorf("unknown event %q", event.Type)
	}
	return event, nil
}

func parseFields(line string) (map[string]string, error) {
	fields := make(map[string]string)
	for _, token := range strings.Fields(line) {
		key, value, ok := strings.Cut(token, "=")
		if !ok || key == "" || !validValue(value) || fields[key] != "" {
			return nil, fmt.Errorf("invalid or duplicate field %q", token)
		}
		fields[key] = value
	}
	if fields["v"] != "1" {
		return nil, fmt.Errorf("unsupported protocol version %q", fields["v"])
	}
	return fields, nil
}

func validValue(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char <= ' ' || char > '~' || char == '=' {
			return false
		}
	}
	return true
}

// Listener owns the listening socket. Only children accept connections.
type Listener struct {
	net.Listener
	File *os.File
}

func OpenListener(network, address string) (*Listener, error) {
	listener, err := net.Listen(network, address)
	if err != nil {
		return nil, err
	}
	file, err := listener.(interface{ File() (*os.File, error) }).File()
	if err != nil {
		listener.Close()
		return nil, err
	}
	result := &Listener{Listener: listener, File: file}
	return result, nil
}

func (listener *Listener) ChildFile() (*os.File, error) {
	return listener.Listener.(interface{ File() (*os.File, error) }).File()
}

func (listener *Listener) Close() error {
	listener.File.Close()
	return listener.Listener.Close()
}

// One epoll instance observes all listeners. The loopback datagram wakes the
// blocking Wait during shutdown on both platforms; it carries no worker data.
type Poller struct {
	fd    int
	wake  net.PacketConn
	waker net.Conn
}

func NewPoller() (*Poller, error) {
	fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	wake, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		syscall.EpollClose(fd)
		return nil, err
	}
	waker, err := net.Dial("udp4", wake.LocalAddr().String())
	if err != nil {
		wake.Close()
		syscall.EpollClose(fd)
		return nil, err
	}
	poller := &Poller{fd: fd, wake: wake, waker: waker}
	raw, err := wake.(syscall.Conn).SyscallConn()
	if err == nil {
		var controlErr error
		err = raw.Control(func(socket uintptr) {
			controlErr = syscall.EpollCtl(fd, syscall.EPOLL_CTL_ADD, int(socket), &syscall.EpollEvent{Events: syscall.EPOLLIN | syscall.EPOLLONESHOT})
		})
		if err == nil {
			err = controlErr
		}
	}
	if err != nil {
		poller.Close()
		return nil, err
	}
	return poller, nil
}

func (poller *Poller) Close() error {
	err := syscall.EpollClose(poller.fd)
	poller.waker.Close()
	poller.wake.Close()
	return err
}

type process struct {
	cmd       *exec.Cmd
	job       *processJob
	service   *service
	ready     bool
	active    map[string]time.Time
	started   time.Time
	idleSince time.Time
	stopping  time.Time
	killed    bool
	failed    bool
	probing   bool
	nextProbe time.Time
	config    App
}

type message struct {
	process    *process
	sockets    []syscall.EpollEvent
	poll       bool
	event      Event
	exited     bool
	probe      bool
	ready      bool
	err        error
	cleanupErr error
}

func (manager *manager) spawn(service *service, now time.Time) error {
	var childFile *os.File
	if service.listener != nil {
		var err error
		childFile, err = service.listener.ChildFile()
		if err != nil {
			return err
		}
		defer childFile.Close()
	}
	app := service.config
	cmd := exec.Command(app.Command[0], app.Command[1:]...)
	cmd.Dir = app.Directory
	releaseIdentity, err := app.Identity.apply(cmd)
	if err != nil {
		return fmt.Errorf("worker identity: %w", err)
	}
	defer releaseIdentity()
	cmd.Env = os.Environ()
	for key, value := range app.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env, "OOTH_WORKER=1", "OOTH_CONCURRENCY="+strconv.Itoa(app.Concurrency))
	if childFile != nil {
		cmd.Stdin = childFile
	}
	cmd.Stderr = os.Stderr
	cmd.NewProcessGroup = true
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = writer
	if app.Ready != "event" {
		cmd.Stdout = os.Stderr
	}
	job, err := manager.owner.Start(cmd)
	if err != nil {
		reader.Close()
		writer.Close()
		return err
	}
	writer.Close()
	cmd = job.cmd
	child := &process{cmd: cmd, job: job, service: service, config: app, active: make(map[string]time.Time), started: now}
	if app.Ready == "started" {
		child.ready = true
		child.idleSince = now
	}
	manager.workers[child] = struct{}{}
	service.workers[child] = struct{}{}
	service.lastSpawn = now
	service.demand = false
	manager.log.Info("worker started", "app", service.name, "pid", cmd.Process.Pid)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		if app.Ready != "event" {
			return
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 4096)
		for scanner.Scan() {
			event, err := ParseEvent(scanner.Text())
			manager.messages <- message{process: child, event: event, err: err}
			if err != nil {
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = fmt.Errorf("worker closed its event stream")
		}
		manager.messages <- message{process: child, err: err}
	}()
	go func() {
		err := manager.owner.Wait(job)
		cleanupErr := job.Close()
		// A descendant must not keep a dead worker's stdout pipe open forever.
		select {
		case <-scanned:
		case <-time.After(100 * time.Millisecond):
			reader.Close()
			<-scanned
		}
		reader.Close()
		manager.messages <- message{process: child, exited: true, err: err, cleanupErr: cleanupErr}
	}()
	return nil
}

type serviceListener struct {
	*Listener
	poller  int
	socket  int
	id      int32
	rearmAt time.Time
}

func (listener *serviceListener) close() error {
	err := syscall.EpollCtl(listener.poller, syscall.EPOLL_CTL_DEL, listener.socket, nil)
	listener.Close()
	return err
}

func (listener *serviceListener) arm(operation int) error {
	return syscall.EpollCtl(listener.poller, operation, listener.socket,
		&syscall.EpollEvent{Events: syscall.EPOLLIN | syscall.EPOLLONESHOT, Fd: listener.id})
}

type service struct {
	name          string
	config        App
	listener      *serviceListener
	workers       map[*process]struct{}
	demand        bool
	lastSpawn     time.Time
	busySince     time.Time
	retryAt       time.Time
	failures      int
	unneededSince time.Time
}

type manager struct {
	owner        *processOwner
	cgroup       string
	services     map[string]*service
	workers      map[*process]struct{}
	messages     chan message
	log          *slog.Logger
	done         chan struct{}
	fatal        error
	poller       *Poller
	listeners    map[int32]*service
	nextListener int32
}

func Run(ctx context.Context, path string, logger *slog.Logger) error {
	initial, err := Load(path)
	if err != nil {
		return err
	}
	owner, err := newProcessOwner(initial.Cgroup, logger)
	if err != nil {
		return err
	}
	defer owner.Close()
	poller, err := NewPoller()
	if err != nil {
		return fmt.Errorf("create listener poller: %w", err)
	}
	defer poller.Close()
	manager := &manager{
		owner: owner, cgroup: initial.Cgroup,
		services: make(map[string]*service), workers: make(map[*process]struct{}),
		messages: make(chan message, 256), log: logger, done: make(chan struct{}),
		poller: poller, listeners: make(map[int32]*service),
	}
	if err := manager.apply(initial); err != nil {
		return err
	}
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); manager.watchListeners() }()
	defer func() {
		close(manager.done)
		poller.waker.Write([]byte{1})
		<-pollDone
	}()
	watchContext, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	updates := make(chan Snapshot)
	watchDone := make(chan error, 1)
	go func() { watchDone <- Watch(watchContext, path, initial, updates) }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	done := ctx.Done()
	stopping := false
	var result error
	var pending *Snapshot
	var retryConfig time.Time
	for {
		if manager.fatal != nil && result == nil {
			result = manager.fatal
		}
		if manager.fatal != nil && !stopping {
			stopping = true
			cancelWatch()
			manager.shutdown()
		}
		if stopping && len(manager.workers) == 0 {
			return result
		}
		select {
		case <-done:
			done = nil
			stopping = true
			cancelWatch()
			manager.shutdown()
		case err := <-watchDone:
			watchDone = nil
			if !stopping {
				if ctx.Err() == nil {
					result = fmt.Errorf("configuration watcher stopped: %v", err)
				}
				stopping = true
				manager.shutdown()
			}
		case next := <-updates:
			if !stopping {
				if err := manager.apply(next); err != nil {
					logger.Error("configuration rejected; keeping current services", "error", err)
					pending = &next
					retryConfig = time.Now().Add(time.Second)
				} else {
					pending = nil
				}
			}
		case msg := <-manager.messages:
			manager.handle(msg, time.Now())
		case now := <-ticker.C:
			if pending != nil && !stopping && !now.Before(retryConfig) {
				if manager.apply(*pending) == nil {
					pending = nil
				}
				retryConfig = now.Add(time.Second)
			}
			manager.tick(now, stopping)
		}
	}
}

func (manager *manager) apply(snapshot Snapshot) error {
	if snapshot.Cgroup != manager.cgroup {
		return fmt.Errorf("changing cgroup requires restarting ooth")
	}
	opened := make(map[string]*serviceListener)
	committed := false
	defer func() {
		if !committed {
			for _, listener := range opened {
				listener.close()
			}
		}
	}()
	for name, app := range snapshot.Apps {
		if app.Listen.Network == "" {
			continue
		}
		previous := manager.services[name]
		if previous != nil && previous.config.Listen == app.Listen {
			continue
		}
		listener, err := OpenListener(app.Listen.Network, app.Listen.Address)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if manager.nextListener == 1<<31-1 {
			listener.Close()
			return fmt.Errorf("listener identifiers exhausted")
		}
		manager.nextListener++
		staged := &serviceListener{Listener: listener, poller: manager.poller.fd,
			socket: int(listener.File.Fd()), id: manager.nextListener}
		if err := staged.arm(syscall.EPOLL_CTL_ADD); err != nil {
			listener.Close()
			return fmt.Errorf("%s: register listener: %w", name, err)
		}
		opened[name] = staged
	}
	for name, previous := range manager.services {
		next, exists := snapshot.Apps[name]
		if exists && reflect.DeepEqual(previous.config, next) {
			continue
		}
		for child := range previous.workers {
			manager.stop(child, time.Now(), "configuration changed")
		}
		if !exists || previous.config.Listen != next.Listen {
			if previous.listener != nil {
				manager.closeListener(previous.listener)
			}
			delete(manager.services, name)
		}
	}
	for name, app := range snapshot.Apps {
		previous := manager.services[name]
		if previous != nil && reflect.DeepEqual(previous.config, app) {
			continue
		}
		listener := opened[name]
		if previous != nil {
			listener = previous.listener
		}
		current := &service{name: name, config: app, listener: listener, workers: make(map[*process]struct{})}
		// Retiring workers count against max_workers until they actually exit.
		if previous != nil {
			for child := range previous.workers {
				child.service = current
				current.workers[child] = struct{}{}
			}
		}
		manager.services[name] = current
		if listener != nil {
			manager.listeners[listener.id] = current
		}
		manager.log.Info("service configured", "app", name, "network", app.Listen.Network, "address", app.Listen.Address)
	}
	committed = true
	return nil
}

func (manager *manager) watchListeners() {
	events := make([]syscall.EpollEvent, 64)
	for {
		count, err := syscall.EpollWait(manager.poller.fd, events, -1)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			count = 0 // Linux returns -1 on failure.
		}
		for _, event := range events[:count] {
			if event.Fd == 0 {
				return
			}
		}
		select {
		case <-manager.done:
			return
		case manager.messages <- message{poll: true, sockets: slices.Clone(events[:count]), err: err}:
		}
		if err != nil {
			return
		}
	}
}

func (manager *manager) closeListener(listener *serviceListener) {
	delete(manager.listeners, listener.id)
	if err := listener.close(); err != nil {
		manager.log.Error("unregister listener", "error", err)
	}
}

func (manager *manager) shutdown() {
	for _, service := range manager.services {
		if service.listener != nil {
			manager.closeListener(service.listener)
			service.listener = nil
		}
	}
	manager.tick(time.Now(), true)
}

func (manager *manager) stop(child *process, now time.Time, reason string) {
	if !child.stopping.IsZero() {
		return
	}
	child.stopping = now
	manager.log.Info("worker stopping", "app", child.service.name, "pid", child.cmd.Process.Pid, "reason", reason)
	if err := child.cmd.Process.Signal(StopSignal); err != nil && !errors.Is(err, os.ErrProcessDone) {
		manager.log.Error("graceful signal failed", "pid", child.cmd.Process.Pid, "error", err)
	}
}

func (manager *manager) handle(msg message, now time.Time) {
	if msg.poll {
		if msg.err != nil {
			manager.fatal = fmt.Errorf("listener poller: %w", msg.err)
			return
		}
		for _, event := range msg.sockets {
			service := manager.listeners[event.Fd]
			if service == nil {
				continue // A queued event can outlive a configuration.
			}
			if event.Events&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
				manager.fatal = fmt.Errorf("%s: listener event %#x", service.name, event.Events)
				continue
			}
			service.demand = true
			service.listener.rearmAt = now.Add(25 * time.Millisecond)
		}
		return
	}
	child := msg.process
	if _, exists := manager.workers[child]; !exists {
		return
	}
	service := child.service
	if msg.exited {
		if msg.cleanupErr != nil {
			manager.fatal = fmt.Errorf("clean up worker %d descendants: %w", child.cmd.Process.Pid, msg.cleanupErr)
		}
		delete(manager.workers, child)
		delete(service.workers, child)
		manager.log.Info("worker exited", "app", service.name, "pid", child.cmd.Process.Pid, "error", msg.err)
		if child.stopping.IsZero() || child.failed {
			service.demand = true
			service.backoff(now)
		}
		return
	}
	if msg.probe {
		child.probing = false
		child.nextProbe = now.Add(100 * time.Millisecond)
		if msg.ready {
			child.ready = true
			child.idleSince = now
		}
		return
	}
	if msg.err != nil {
		if child.stopping.IsZero() {
			child.failed = true
			manager.log.Warn("worker event stream failed", "pid", child.cmd.Process.Pid, "error", msg.err)
			manager.stop(child, now, msg.err.Error())
		}
		return
	}
	event := msg.event
	switch event.Type {
	case "ready":
		if child.ready {
			child.failed = true
			manager.stop(child, now, "duplicate ready event")
			return
		}
		child.ready = true
		child.idleSince = now
	case "start":
		if !child.ready || event.ID == "" || !child.active[event.ID].IsZero() {
			child.failed = true
			manager.stop(child, now, "invalid request start")
			return
		}
		child.active[event.ID] = now
		child.idleSince = time.Time{}
	case "end":
		if child.active[event.ID].IsZero() {
			child.failed = true
			manager.stop(child, now, "request end without start")
			return
		}
		delete(child.active, event.ID)
		if len(child.active) == 0 {
			child.idleSince = now
		}
		manager.log.Debug("request finished", "app", service.name, "pid", child.cmd.Process.Pid,
			"id", event.ID, "ts", event.Time, "duration_ns", event.DurationNS)
	}
}

func (service *service) backoff(now time.Time) {
	service.failures++
	delay := 250 * time.Millisecond * time.Duration(1<<min(service.failures-1, 7))
	service.retryAt = now.Add(delay)
}

func (manager *manager) tick(now time.Time, stopping bool) {
	if !stopping {
		for _, service := range manager.listeners {
			listener := service.listener
			if !listener.rearmAt.IsZero() && !now.Before(listener.rearmAt) {
				if err := listener.arm(syscall.EPOLL_CTL_MOD); err != nil {
					manager.fatal = fmt.Errorf("%s: rearm listener: %w", service.name, err)
				}
				listener.rearmAt = time.Time{}
			}
		}
	}
	for child := range manager.workers {
		app := child.config
		if !child.stopping.IsZero() {
			if !child.killed && now.Sub(child.stopping) >= app.StopTimeout {
				child.killed = true
				manager.log.Warn("worker exceeded graceful timeout", "pid", child.cmd.Process.Pid)
				if err := child.job.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					manager.log.Error("worker kill failed", "error", err)
				}
			}
			continue
		}
		if !child.ready && now.Sub(child.started) >= app.StartTimeout {
			child.failed = true
			manager.stop(child, now, "startup timeout")
			child.service.backoff(now)
			child.service.demand = true
		}
		if !child.ready && app.Ready != "event" && !child.probing && !now.Before(child.nextProbe) && child.stopping.IsZero() {
			child.probing = true
			go func() {
				network, address, _ := strings.Cut(app.Ready, "://")
				connection, err := net.DialTimeout(network, address, 100*time.Millisecond)
				if err == nil {
					connection.Close()
				}
				select {
				case manager.messages <- message{process: child, probe: true, ready: err == nil}:
				case <-manager.done:
				}
			}()
		}
		if app.RequestTimeout > 0 {
			for _, started := range child.active {
				if now.Sub(started) >= app.RequestTimeout {
					child.failed = true
					manager.stop(child, now, "request timeout")
				}
			}
		}
		if child.ready && now.Sub(child.started) >= 10*time.Second {
			child.service.failures = 0
		}
	}
	if stopping {
		for child := range manager.workers {
			required := false
			for dependent := range manager.workers {
				if slices.Contains(dependent.config.Requires, child.service.name) {
					required = true
					break
				}
			}
			if !required {
				manager.stop(child, now, "supervisor stopping")
			}
		}
		return
	}
	// A virtual activation holds dependencies while a caller is starting or
	// running. Shared prerequisites start once; independent branches run together.
	wanted := make(map[string]bool)
	required := make(map[string]bool)
	var activate func(string)
	activate = func(name string) {
		if wanted[name] {
			return
		}
		current := manager.services[name]
		if current == nil {
			return
		}
		wanted[name] = true
		for _, dependency := range current.config.Requires {
			required[dependency] = true
			activate(dependency)
		}
	}
	for name, current := range manager.services {
		if current.config.Startup || current.config.MinWorkers > 0 || current.demand || (current.listener != nil && len(current.workers) > 0) {
			activate(name)
		}
	}
	for _, service := range manager.services {
		app := service.config
		dependenciesReady := true
		for _, name := range app.Requires {
			dependency := manager.services[name]
			ready := false
			if dependency != nil {
				for child := range dependency.workers {
					if child.ready && child.stopping.IsZero() {
						ready = true
					}
				}
			}
			dependenciesReady = dependenciesReady && ready
		}
		if !dependenciesReady {
			if len(service.workers) > 0 {
				service.demand = true
			}
			for child := range service.workers {
				manager.stop(child, now, "dependency unavailable")
			}
			continue
		}
		available, starting, busy := 0, 0, 0
		for child := range service.workers {
			if !child.stopping.IsZero() {
				continue
			}
			available++
			if !child.ready {
				starting++
			} else if len(child.active) >= app.Concurrency {
				busy++
			}
		}
		if available > 0 && busy == available {
			if service.busySince.IsZero() {
				service.busySince = now
			}
		} else {
			service.busySince = time.Time{}
		}
		grow := available < app.MinWorkers || (available == 0 && service.demand)
		if available == 0 && wanted[service.name] {
			grow = true
		}
		grow = grow || (!service.busySince.IsZero() && now.Sub(service.busySince) >= app.ScaleDelay)
		if grow && starting == 0 && len(service.workers) < app.MaxWorkers && !now.Before(service.retryAt) && now.Sub(service.lastSpawn) >= app.ScaleDelay {
			if err := manager.spawn(service, now); err != nil {
				service.backoff(now)
				manager.log.Error("worker start failed", "app", service.name, "error", err)
			}
		}
		if available > 0 {
			service.demand = false
		}
		if wanted[service.name] {
			service.unneededSince = time.Time{}
		} else if service.unneededSince.IsZero() {
			service.unneededSince = now
		}
		for child := range service.workers {
			minimum := app.MinWorkers
			if required[service.name] {
				minimum = max(minimum, 1)
			}
			if available <= minimum {
				break
			}
			idle := child.config.Ready == "event" && len(child.active) == 0 && now.Sub(child.idleSince) >= app.IdleTimeout
			if service.listener == nil {
				idle = !wanted[service.name] && now.Sub(service.unneededSince) >= app.IdleTimeout
			}
			if service.config.Startup && available <= 1 {
				idle = false
			}
			if child.stopping.IsZero() && child.ready && idle {
				manager.stop(child, now, "idle timeout")
				available--
			}
		}
	}
}
