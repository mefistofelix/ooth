package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/sgtdi/fswatcher"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
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
	Watch     []string       `yaml:"watch"`
	Cgroup    string         `yaml:"cgroup"`
	Resources ResourceLimits `yaml:"resources"`
}

type ResourceLimits struct {
	MaxCPUPercent             Percent `yaml:"max_cpu_percent"`
	MinAvailableMemoryPercent Percent `yaml:"min_available_memory_percent"`
}

type Percent float64

func (percent *Percent) UnmarshalYAML(data []byte) error {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return err
	}
	number, err := strconv.ParseFloat(strings.TrimSuffix(fmt.Sprint(value), "%"), 64)
	if err != nil || math.IsNaN(number) || number < 0 || number > 100 {
		return fmt.Errorf("percentage must be between 0 and 100")
	}
	*percent = Percent(number)
	return nil
}

type resourceSample struct {
	at                   time.Time
	cpu, availableMemory float64
	err                  error
}

type resourceGroup struct {
	cpuSeconds, cores float64
	availableMemory   float64
}

func (limits ResourceLimits) blocked(sample resourceSample, now time.Time) string {
	if limits.MaxCPUPercent == 0 && limits.MinAvailableMemoryPercent == 0 {
		return ""
	}
	if sample.at.IsZero() || now.Sub(sample.at) > 3*time.Second {
		return "waiting for a fresh resource sample"
	}
	if sample.err != nil {
		return "resource measurements unavailable"
	}
	if limits.MaxCPUPercent > 0 && sample.cpu >= float64(limits.MaxCPUPercent) {
		return "CPU limit reached"
	}
	if limits.MinAvailableMemoryPercent > 0 && sample.availableMemory < float64(limits.MinAvailableMemoryPercent) {
		return "available memory below reserve"
	}
	return ""
}

// Only this sampler blocks for the CPU interval; the supervisor keeps serving
// process exits, deadlines and listener events while it runs.
func sampleResources(ctx context.Context, owner *processOwner, samples chan<- resourceSample) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		started := time.Now()
		before, beforeErr := owner.resourceGroups()
		usage, cpuErr := cpu.PercentWithContext(ctx, time.Second, false)
		memory, memoryErr := mem.VirtualMemoryWithContext(ctx)
		after, afterErr := owner.resourceGroups()
		sample := resourceSample{at: time.Now(), err: errors.Join(beforeErr, cpuErr, memoryErr, afterErr)}
		if sample.err == nil {
			sample.cpu = usage[0]
			sample.availableMemory = 100 * float64(memory.Available) / float64(memory.Total)
			for path, group := range after {
				sample.availableMemory = min(sample.availableMemory, group.availableMemory)
				previous, ok := before[path]
				if group.cores > 0 {
					if !ok || group.cpuSeconds < previous.cpuSeconds {
						sample.err = fmt.Errorf("cgroup CPU counter changed during sampling: %s", path)
						break
					}
					sample.cpu = max(sample.cpu, 100*(group.cpuSeconds-previous.cpuSeconds)/sample.at.Sub(started).Seconds()/group.cores)
				}
			}
		}
		select {
		case samples <- sample:
		case <-ctx.Done():
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
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

type Permissions uint32

func (mode *Permissions) UnmarshalYAML(data []byte) error {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return err
	}
	var bits uint64
	var err error
	switch value := value.(type) {
	case uint64:
		bits = value
	case int64:
		bits = uint64(value)
	case string:
		bits, err = parsePermissions(value)
	default:
		err = fmt.Errorf("socket_mode must be octal or symbolic permissions")
	}
	if err != nil {
		return err
	}
	if bits > 0777 {
		return fmt.Errorf("socket_mode must contain only rwx permission bits")
	}
	*mode = Permissions(bits)
	return nil
}

func (mode Permissions) MarshalYAML() (any, error) {
	return fmt.Sprintf("%04o", mode), nil
}

func parsePermissions(value string) (uint64, error) {
	if bits, err := strconv.ParseUint(strings.TrimPrefix(value, "0o"), 8, 32); err == nil {
		return bits, nil
	}
	mode := uint64(0660)
	invalid := fmt.Errorf("socket_mode requires octal or clauses like u=rw,g=rw,o= using u/g/o/a, =/+/- and r/w/x")
	for _, clause := range strings.Split(value, ",") {
		index := strings.IndexAny(clause, "=+-")
		if index < 1 {
			return 0, invalid
		}
		var mask, bits uint64
		for _, who := range clause[:index] {
			switch who {
			case 'u':
				mask |= 0700
			case 'g':
				mask |= 0070
			case 'o':
				mask |= 0007
			case 'a':
				mask |= 0777
			default:
				return 0, invalid
			}
		}
		for _, permission := range clause[index+1:] {
			switch permission {
			case 'r':
				bits |= 0444
			case 'w':
				bits |= 0222
			case 'x':
				bits |= 0111
			default:
				return 0, invalid
			}
		}
		bits &= mask
		switch clause[index] {
		case '=':
			mode = mode&^mask | bits
		case '+':
			mode |= bits
		case '-':
			mode &^= bits
		}
	}
	return mode, nil
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
	Vars           map[string]string `yaml:"vars,omitempty"`
	RestartOn      []RestartRule     `yaml:"restart_on,omitempty"`
	Listen         Socket            `yaml:"listen"`
	SocketHandoff  string            `yaml:"socket_handoff,omitempty"`
	SocketMode     *Permissions      `yaml:"socket_mode"`
	MinWorkers     int               `yaml:"min_workers"`
	MaxWorkers     int               `yaml:"max_workers"`
	Concurrency    int               `yaml:"concurrency"`
	IdleTimeout    time.Duration     `yaml:"idle_timeout"`
	StartTimeout   time.Duration     `yaml:"start_timeout"`
	StopTimeout    time.Duration     `yaml:"stop_timeout"`
	RequestTimeout time.Duration     `yaml:"request_timeout"`
	ScaleAt        Percent           `yaml:"scale_at,omitempty"`
	ScaleWindow    time.Duration     `yaml:"scale_window,omitempty"`
	ScaleDelay     time.Duration     `yaml:"scale_delay,omitempty"` // Legacy spelling of scale_window.
	Source         string            `yaml:"-"`
}

type RestartRule struct {
	Glob   string   `yaml:"glob"`
	Events []string `yaml:"events,omitempty"`
}

var fileEvents = map[string]fswatcher.EventType{
	"create": fswatcher.EventCreate, "write": fswatcher.EventMod,
	"remove": fswatcher.EventRemove, "rename": fswatcher.EventRename,
	"chmod": fswatcher.EventChmod,
}

func (rule RestartRule) matches(event fswatcher.WatchEvent) bool {
	path := filepath.Clean(event.Path)
	matched := matchGlob(rule.Glob, path)
	// A replaced directory can change every matching file below it without
	// individual child events, including files created before a new watch is ready.
	if slices.Contains(event.Types, fswatcher.EventCreate) || slices.Contains(event.Types, fswatcher.EventRemove) || slices.Contains(event.Types, fswatcher.EventRename) {
		for parent := filepath.Dir(rule.Glob); !matched && filepath.Dir(parent) != parent; parent = filepath.Dir(parent) {
			matched = matchGlob(parent, path)
		}
	}
	if !matched {
		return false
	}
	for _, name := range rule.Events {
		if slices.Contains(event.Types, fileEvents[name]) {
			return true
		}
	}
	return false
}

type Snapshot struct {
	Apps      map[string]App
	Roots     []string
	Cgroup    string
	Resources ResourceLimits
}

// Load validates the whole snapshot before it can replace running services.
// Relative paths resolve against the YAML file that contains them.
func Load(path string) (Snapshot, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Snapshot{}, err
	}
	main := Config{Resources: ResourceLimits{MaxCPUPercent: 90, MinAvailableMemoryPercent: 10}}
	if err := read(path, &main); err != nil {
		return Snapshot{}, err
	}
	if len(main.Watch) == 0 {
		return Snapshot{}, fmt.Errorf("%s: watch must contain at least one glob", path)
	}
	if main.Resources.MaxCPUPercent < 0 || main.Resources.MaxCPUPercent > 100 || main.Resources.MinAvailableMemoryPercent < 0 || main.Resources.MinAvailableMemoryPercent > 100 {
		return Snapshot{}, fmt.Errorf("resource percentages must be between 0 and 100; zero disables a limit")
	}
	result := Snapshot{Apps: make(map[string]App), Roots: []string{filepath.Dir(path)}, Resources: main.Resources}
	if main.Cgroup != "" {
		result.Cgroup = resolve(filepath.Dir(path), main.Cgroup)
	}
	sockets := make(map[Socket]string)
	for _, pattern := range main.Watch {
		pattern = resolve(filepath.Dir(path), pattern)
		root, err := watchRoot(pattern)
		if err != nil {
			return Snapshot{}, err
		}
		matches, err := expandGlob(pattern, root)
		if err != nil {
			return Snapshot{}, fmt.Errorf("glob %q: %w", pattern, err)
		}
		result.Roots = append(result.Roots, root)
		for _, file := range matches {
			app := App{
				MaxWorkers: 1, Concurrency: 1, IdleTimeout: time.Minute,
				StartTimeout: 10 * time.Second, StopTimeout: 10 * time.Second,
				ScaleAt: 80,
			}
			if err := read(file, &app); err != nil {
				return Snapshot{}, err
			}
			if app.ScaleWindow != 0 && app.ScaleDelay != 0 {
				return Snapshot{}, fmt.Errorf("%s: use scale_window or the legacy scale_delay, not both", file)
			}
			if app.ScaleWindow == 0 {
				app.ScaleWindow = app.ScaleDelay
				if app.ScaleWindow == 0 {
					app.ScaleWindow = time.Second
				}
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
			}
			if err := app.validate(); err != nil {
				return Snapshot{}, fmt.Errorf("%s: %w", file, err)
			}
			app.Directory = resolve(filepath.Dir(file), app.Directory)
			if !strings.Contains(app.Command[0], "{{") && strings.ContainsAny(app.Command[0], `/\`) {
				app.Command[0] = resolve(app.Directory, app.Command[0])
			}
			if app.Listen.Network == "unix" {
				app.Listen.Address = resolve(filepath.Dir(file), app.Listen.Address)
			}
			if _, _, err := app.expandLaunch(app.Listen.Address); err != nil {
				return Snapshot{}, fmt.Errorf("%s: %w", file, err)
			}
			for index := range app.RestartOn {
				rule := &app.RestartOn[index]
				if rule.Glob == "" {
					return Snapshot{}, fmt.Errorf("%s: restart_on requires a glob", file)
				}
				rule.Glob = resolve(filepath.Dir(file), rule.Glob)
				root, err := watchRoot(rule.Glob)
				if err != nil {
					return Snapshot{}, fmt.Errorf("%s: restart_on: %w", file, err)
				}
				if len(rule.Events) == 0 {
					rule.Events = []string{"create", "write", "remove", "rename"}
				}
				for _, event := range rule.Events {
					if _, ok := fileEvents[event]; !ok {
						return Snapshot{}, fmt.Errorf("%s: restart_on event %q must be create, write, remove, rename or chmod", file, event)
					}
				}
				result.Roots = append(result.Roots, root)
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
	if app.SocketMode != nil && (runtime.GOOS != "linux" || app.Listen.Network != "unix" || *app.SocketMode > 0777) {
		return fmt.Errorf("socket_mode requires a Linux Unix socket and permissions between 0000 and 0777")
	}
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
	if app.SocketHandoff != "" && app.SocketHandoff != "env" && app.SocketHandoff != "stdin" && app.SocketHandoff != "0" {
		fd, err := strconv.Atoi(app.SocketHandoff)
		if err != nil || fd < 3 || fd > 1024 {
			return fmt.Errorf("socket_handoff must be env, stdin/0, or a Linux fd from 3 to 1024; stdout/stderr are reserved")
		}
		if runtime.GOOS != "linux" {
			return fmt.Errorf("numeric socket_handoff requires Linux; use env for a native Windows handle")
		}
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
	if app.IdleTimeout <= 0 || app.StartTimeout <= 0 || app.StopTimeout <= 0 || app.ScaleWindow <= 0 || app.ScaleDelay < 0 || app.RequestTimeout < 0 {
		return fmt.Errorf("timeouts and scale_window must be positive; request_timeout may be zero")
	}
	if app.ScaleAt <= 0 || app.ScaleAt > 100 {
		return fmt.Errorf("scale_at must be greater than 0 and at most 100")
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
	decoder := yaml.NewDecoder(file)
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

// A complete ** component matches zero or more directory levels. Other
// components retain filepath.Match syntax and platform-specific separators.
func matchGlob(pattern, path string) bool {
	var match func([]string, []string) bool
	match = func(pattern, path []string) bool {
		for len(pattern) > 0 {
			if pattern[0] == "**" {
				for index := 0; index <= len(path); index++ {
					if match(pattern[1:], path[index:]) {
						return true
					}
				}
				return false
			}
			if len(path) == 0 {
				return false
			}
			matched, _ := filepath.Match(pattern[0], path[0])
			if !matched {
				return false
			}
			pattern, path = pattern[1:], path[1:]
		}
		return len(path) == 0
	}
	return match(strings.Split(filepath.Clean(pattern), string(filepath.Separator)), strings.Split(filepath.Clean(path), string(filepath.Separator)))
}

func expandGlob(pattern, root string) ([]string, error) {
	if !slices.Contains(strings.Split(pattern, string(filepath.Separator)), "**") {
		return filepath.Glob(pattern)
	}
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil // A file may disappear between directory reads.
		}
		if err != nil {
			return err
		}
		if matchGlob(pattern, path) {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

// Watch the nearest existing directory, so globs also cover future files and
// atomic replacements instead of following just today's matching inodes.
func watchRoot(pattern string) (string, error) {
	// path.Match checks the entire pattern even after an unmatched prefix.
	if _, err := path.Match(filepath.ToSlash(pattern), ""); err != nil {
		return "", fmt.Errorf("glob %q: %w", pattern, err)
	}
	root := pattern
	if index := strings.IndexAny(root, "*?["); index >= 0 {
		root = root[:index]
	}
	root = filepath.Dir(root)
	for {
		info, err := os.Stat(root)
		if err == nil && info.IsDir() {
			return root, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("cannot watch %q", pattern)
		}
		root = parent
	}
}

// Expand each argument/value independently: no shell, word splitting or recursive
// environment expansion. Keep the stored configuration unchanged for reloads.
func (app App) expandLaunch(address string) ([]string, map[string]string, error) {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	values := map[string]any{"name": app.Name, "directory": app.Directory, "source": app.Source, "env": environment, "vars": app.Vars}
	if app.Listen.Network != "" {
		endpoint := map[string]string{"network": app.Listen.Network, "address": address}
		uri := url.URL{Scheme: "tcp", Host: address}
		if app.Listen.Network == "unix" {
			endpoint["path"] = address
			uri = url.URL{Scheme: "unix", Path: filepath.ToSlash(address)}
			if !strings.HasPrefix(uri.Path, "/") {
				uri.Path = "/" + uri.Path
			}
		} else {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, nil, fmt.Errorf("listen.address: %w", err)
			}
			endpoint["host"], endpoint["port"] = host, port
		}
		endpoint["url"] = uri.String()
		values["listen"] = endpoint
	}
	expand := func(field, value string) (string, error) {
		if !strings.Contains(value, "{{") {
			return value, nil
		}
		compiled, err := template.New(field).Option("missingkey=error").Parse(value)
		if err != nil {
			return "", fmt.Errorf("invalid template in %s", field)
		}
		var result strings.Builder
		if err := compiled.Execute(&result, values); err != nil {
			return "", fmt.Errorf("cannot expand template in %s: check placeholder names and environment", field)
		}
		if strings.ContainsRune(result.String(), 0) {
			return "", fmt.Errorf("NUL in expanded %s", field)
		}
		return result.String(), nil
	}
	command := make([]string, len(app.Command))
	for index, argument := range app.Command {
		value, err := expand(fmt.Sprintf("command[%d]", index), argument)
		if err != nil {
			return nil, nil, err
		}
		command[index] = value
	}
	if len(command) == 0 || command[0] == "" {
		return nil, nil, fmt.Errorf("expanded command must name an executable")
	}
	if strings.ContainsAny(command[0], `/\`) {
		command[0] = resolve(app.Directory, command[0])
	}
	childEnv := make(map[string]string, len(app.Env))
	for key, value := range app.Env {
		expanded, err := expand("env."+key, value)
		if err != nil {
			return nil, nil, err
		}
		childEnv[key] = expanded
	}
	return command, childEnv, nil
}

type watchUpdate struct {
	snapshot *Snapshot
	restarts map[string]App
}

// Watch reconciles configuration and batches per-app file restart requests.
// Event loss retires watched workers conservatively; configuration still rescans.
func Watch(ctx context.Context, path string, initial Snapshot, updates chan<- watchUpdate) error {
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
	restarts := make(map[string]App)
	var lost int64
	record := func(event fswatcher.WatchEvent) {
		overflow := slices.Contains(event.Types, fswatcher.EventOverflow)
		if overflow {
			slog.Warn("file events lost; restarting active apps with restart_on rules")
		}
		for name, app := range initial.Apps {
			for _, rule := range app.RestartOn {
				if overflow || rule.matches(event) {
					restarts[name] = app
					break
				}
			}
		}
		debounce.Reset(100 * time.Millisecond)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case event, ok := <-watcher.Events():
			if !ok {
				return <-done
			}
			record(event)
			continue
		case event, ok := <-watcher.Dropped():
			if !ok {
				return <-done
			}
			record(event)
			continue
		case <-debounce.C:
		case <-periodic.C:
		}
		if count := watcher.Stats().EventsLost; count > lost {
			record(fswatcher.WatchEvent{Types: []fswatcher.EventType{fswatcher.EventOverflow}})
			lost = count
		}
		update := watchUpdate{restarts: restarts}
		next, err := Load(path)
		if err != nil {
			slog.Error("configuration rejected; keeping current services", "error", err)
		} else if !slices.Equal(initial.Roots, next.Roots) {
			replacement, replacementDone, err := startWatcher(ctx, next.Roots)
			if err != nil {
				slog.Error("watch update rejected", "error", err)
			} else {
				watcher.Close()
				<-done
				watcher, done = replacement, replacementDone
				lost = 0
				update.snapshot = &next
			}
		} else if !reflect.DeepEqual(initial, next) {
			update.snapshot = &next
		}
		if update.snapshot == nil && len(restarts) == 0 {
			continue
		}
		select {
		case updates <- update:
			if update.snapshot != nil {
				initial = next
			}
			restarts = make(map[string]App)
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
	control   *os.File
	job       *processJob
	service   *service
	ready     bool
	telemetry bool
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
	observed   time.Time
	event      Event
	exited     bool
	probe      bool
	handshake  bool
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
	address := app.Listen.Address
	if service.listener != nil {
		address = service.listener.Addr().String()
	}
	command, environment, err := app.expandLaunch(address)
	if err != nil {
		return err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = app.Directory
	releaseIdentity, err := app.Identity.apply(cmd)
	if err != nil {
		return fmt.Errorf("worker identity: %w", err)
	}
	defer releaseIdentity()
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(variable), "OOTH_LISTEN_HANDLE=") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	for key, value := range environment {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env, "OOTH_WORKER=1", "OOTH_CONCURRENCY="+strconv.Itoa(app.Concurrency))
	var control *os.File
	if childFile != nil && (app.SocketHandoff == "stdin" || app.SocketHandoff == "0") {
		cmd.Stdin = childFile
	} else {
		if childFile != nil {
			fd, _ := strconv.Atoi(app.SocketHandoff)
			if fd == 0 {
				fd = 3
			}
			handle, err := inheritListener(cmd, childFile, fd)
			if err != nil {
				return err
			}
			cmd.Env = append(cmd.Env, "OOTH_LISTEN_HANDLE="+strconv.FormatUint(uint64(handle), 10))
		}
		input, output, err := os.Pipe()
		if err != nil {
			return err
		}
		defer input.Close()
		control = output
		defer func() {
			if control != nil { // Start failed: the process never took ownership.
				control.Close()
			}
		}()
		cmd.Stdin = input
	}
	cmd.Stderr = os.Stderr
	cmd.NewProcessGroup = true
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = writer
	job, err := manager.owner.Start(cmd)
	if err != nil {
		reader.Close()
		writer.Close()
		return err
	}
	writer.Close()
	cmd = job.cmd
	child := &process{cmd: cmd, control: control, job: job, service: service, config: app, active: make(map[string]time.Time), started: now}
	control = nil
	if app.Ready == "started" {
		child.ready = true
		child.idleSince = now
	}
	manager.workers[child] = struct{}{}
	service.workers[child] = struct{}{}
	service.demand = false
	manager.log.Info("worker started", "app", service.name, "pid", cmd.Process.Pid)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		buffered := bufio.NewReaderSize(reader, 4096)
		first, readErr := buffered.ReadSlice('\n')
		event, parseErr := ParseEvent(string(first))
		protocol := readErr == nil && parseErr == nil && event.Type == "ready"
		manager.messages <- message{process: child, handshake: true, ready: protocol}
		if !protocol {
			os.Stderr.Write(first)
			io.Copy(os.Stderr, buffered)
			return
		}
		scanner := bufio.NewScanner(buffered)
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
		if child.control != nil {
			child.control.Close()
		}
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
	pressure      pressureWindow
	retryAt       time.Time
	failures      int
	unneededSince time.Time
}

// Integrate occupancy at every telemetry transition, including short requests
// that start and finish between scheduler ticks. Capacity changes reset it.
type pressureWindow struct {
	since, last time.Time
	value, area float64
	workers     int
}

func (service *service) observePressure(now time.Time) {
	count, active := 0, 0
	for child := range service.workers {
		if !child.stopping.IsZero() {
			continue
		}
		if !child.ready || !child.telemetry {
			service.pressure = pressureWindow{}
			return
		}
		count++
		active += min(len(child.active), service.config.Concurrency)
	}
	if count == 0 {
		service.pressure = pressureWindow{}
		return
	}
	pressure := &service.pressure
	if pressure.since.IsZero() || pressure.workers != count {
		*pressure = pressureWindow{since: now, last: now, workers: count}
	}
	pressure.area += pressure.value * now.Sub(pressure.last).Seconds()
	pressure.last = now
	pressure.value = float64(active) / float64(count*service.config.Concurrency)
}

func (pressure *pressureWindow) evaluate(now time.Time, window time.Duration, threshold Percent) bool {
	if pressure.since.IsZero() || now.Sub(pressure.since) < window {
		return false
	}
	average := 100 * pressure.area / now.Sub(pressure.since).Seconds()
	pressure.since, pressure.area = now, 0
	return average >= float64(threshold)
}

type manager struct {
	owner          *processOwner
	cgroup         string
	services       map[string]*service
	workers        map[*process]struct{}
	messages       chan message
	log            *slog.Logger
	done           chan struct{}
	fatal          error
	poller         *Poller
	listeners      map[int32]*service
	nextListener   int32
	resourceLimits ResourceLimits
	resources      resourceSample
	resourceBlock  string
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
	resourceContext, cancelResources := context.WithCancel(ctx)
	samples := make(chan resourceSample)
	resourcesDone := make(chan struct{})
	go func() { defer close(resourcesDone); sampleResources(resourceContext, owner, samples) }()
	defer func() { cancelResources(); <-resourcesDone }()
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); manager.watchListeners() }()
	defer func() {
		close(manager.done)
		poller.waker.Write([]byte{1})
		<-pollDone
	}()
	watchContext, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	updates := make(chan watchUpdate)
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
		case sample := <-samples:
			if sample.err != nil && manager.resources.err == nil {
				logger.Warn("resource measurements unavailable; extra worker growth deferred", "error", sample.err)
			}
			manager.resources = sample
			logger.Debug("system resources", "cpu_percent", sample.cpu, "available_memory_percent", sample.availableMemory, "error", sample.err)
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
		case update := <-updates:
			if !stopping {
				if update.snapshot != nil {
					if err := manager.apply(*update.snapshot); err != nil {
						logger.Error("configuration rejected; keeping current services", "error", err)
						pending = update.snapshot
						retryConfig = time.Now().Add(time.Second)
					} else {
						pending = nil
					}
				}
				for name, config := range update.restarts {
					current := manager.services[name]
					if current != nil && reflect.DeepEqual(current.config, config) {
						manager.restart(current, time.Now())
					}
				}
			}
		case msg := <-manager.messages:
			manager.handle(msg, time.Now())
		case <-ticker.C:
			now := time.Now()
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

func (manager *manager) restart(service *service, now time.Time) {
	active := false
	for child := range service.workers {
		if child.stopping.IsZero() {
			active = true
			manager.stop(child, now, "watched files changed")
		}
	}
	if active {
		service.failures = 0
		service.retryAt = time.Time{}
		manager.log.Info("service restarting after file event", "app", service.name)
	}
}

func (manager *manager) apply(snapshot Snapshot) error {
	if snapshot.Cgroup != manager.cgroup {
		return fmt.Errorf("changing cgroup requires restarting ooth")
	}
	opened := make(map[string]*serviceListener)
	var permissions []func(bool) error
	committed := false
	defer func() {
		for index := len(permissions) - 1; index >= 0; index-- {
			if err := permissions[index](committed); err != nil {
				manager.fatal = fmt.Errorf("finish socket permissions: %w", err)
			}
		}
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
	for name, app := range snapshot.Apps {
		if app.Listen.Network != "unix" {
			continue
		}
		previous := manager.services[name]
		if previous != nil && previous.config.Listen == app.Listen && previous.config.Identity.User == app.Identity.User && previous.config.Identity.Group == app.Identity.Group && reflect.DeepEqual(previous.config.SocketMode, app.SocketMode) {
			continue
		}
		mode := os.FileMode(0660)
		if app.SocketMode != nil {
			mode = os.FileMode(*app.SocketMode)
		}
		finish, err := app.Identity.socket(app.Listen.Address, mode)
		if err != nil {
			return fmt.Errorf("%s: socket permissions: %w", name, err)
		}
		permissions = append(permissions, finish)
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
		// Retain draining workers for cleanup, separately from active capacity.
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
	manager.resourceLimits = snapshot.Resources
	committed = true
	return nil
}

func (manager *manager) watchListeners() {
	events := make([]syscall.EpollEvent, 64)
	for {
		count, err := syscall.EpollWait(manager.poller.fd, events, -1)
		observed := time.Now()
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
		case manager.messages <- message{poll: true, observed: observed, sockets: slices.Clone(events[:count]), err: err}:
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
	defer child.service.observePressure(now)
	if !child.stopping.IsZero() {
		return
	}
	child.stopping = now
	if child.failed {
		// Replacements can start before this process exits; throttle failures now.
		child.service.demand = true
		child.service.backoff(now)
	}
	manager.log.Info("worker stopping", "app", child.service.name, "pid", child.cmd.Process.Pid, "reason", reason)
	if child.telemetry && child.control != nil {
		// One short write per process fits in the empty stdin pipe; no reader
		// cooperation is needed to return. The existing deadline bounds draining.
		if _, err := fmt.Fprintf(child.control, "v=1 event=stop ts=%d\n", now.UnixNano()); err == nil {
			return
		} else {
			manager.log.Warn("stdin stop failed; sending signal", "pid", child.cmd.Process.Pid, "error", err)
		}
	}
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
			manager.log.Debug("listener readable", "app", service.name, "observed_ns", msg.observed.UnixNano())
			service.listener.rearmAt = now.Add(25 * time.Millisecond)
		}
		return
	}
	child := msg.process
	if _, exists := manager.workers[child]; !exists {
		return
	}
	service := child.service
	defer service.observePressure(now)
	if msg.exited {
		if msg.cleanupErr != nil {
			manager.fatal = fmt.Errorf("clean up worker %d descendants: %w", child.cmd.Process.Pid, msg.cleanupErr)
		}
		delete(manager.workers, child)
		delete(service.workers, child)
		manager.log.Info("worker exited", "app", service.name, "pid", child.cmd.Process.Pid, "error", msg.err)
		if child.stopping.IsZero() {
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
	if msg.handshake {
		child.telemetry = msg.ready
		if msg.ready {
			if child.config.Ready == "event" {
				child.ready = true
			}
			child.idleSince = now
		} else if child.config.Ready == "event" {
			child.failed = true
			manager.stop(child, now, "missing ready handshake")
		}
		manager.log.Info("worker stdout detected", "app", service.name, "pid", child.cmd.Process.Pid, "telemetry", child.telemetry)
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
		child.failed = true
		manager.stop(child, now, "duplicate ready event")
	case "start":
		if !child.telemetry || event.ID == "" || !child.active[event.ID].IsZero() {
			child.failed = true
			manager.stop(child, now, "invalid request start")
			return
		}
		child.active[event.ID] = now
		child.idleSince = time.Time{}
		manager.log.Debug("request started", "app", service.name, "pid", child.cmd.Process.Pid, "id", event.ID, "ts", event.Time)
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
		if child.ready && child.stopping.IsZero() && now.Sub(child.started) >= 10*time.Second {
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
	blocked := manager.resourceLimits.blocked(manager.resources, now)
	if blocked != manager.resourceBlock {
		manager.resourceBlock = blocked
		if blocked != "" {
			manager.log.Info("extra worker growth paused", "reason", blocked)
		} else {
			manager.log.Info("extra worker growth allowed")
		}
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
		service.observePressure(now)
		pressureGrowth := service.pressure.evaluate(now, app.ScaleWindow, app.ScaleAt)
		available, starting := 0, 0
		for child := range service.workers {
			if !child.stopping.IsZero() {
				continue
			}
			available++
			if !child.ready {
				starting++
			}
		}
		grow := available < app.MinWorkers || (available == 0 && service.demand)
		if available == 0 && (app.Startup || required[service.name]) {
			grow = true
		}
		grow = grow || (blocked == "" && pressureGrowth)
		if grow && starting == 0 && available < app.MaxWorkers && !now.Before(service.retryAt) {
			if err := manager.spawn(service, now); err != nil {
				service.backoff(now)
				manager.log.Error("worker start failed", "app", service.name, "error", err)
			}
			service.observePressure(now)
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
			idle := child.telemetry && len(child.active) == 0 && now.Sub(child.idleSince) >= app.IdleTimeout
			if app.Listen.Network == "" && !child.telemetry {
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
