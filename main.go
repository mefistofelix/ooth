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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
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
	serviceName := flag.String("service", "", "run as the named Windows SCM service")
	logPath := flag.String("log-file", "", "append logs to a file instead of stderr")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logOutput := os.Stderr
	if *logPath != "" {
		var err error
		logOutput, err = os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer logOutput.Close()
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	if *check {
		if _, err := Load(*path); err != nil {
			logger.Error("invalid configuration", "error", err)
			os.Exit(1)
		}
		fmt.Println("configuration valid")
		return
	}
	if *serviceName != "" {
		if err := runSCM(*serviceName, *path, logger); err != nil {
			logger.Error("SCM service stopped", "error", err)
			os.Exit(1)
		}
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
	Identity       Identity             `yaml:",inline"`
	Name           string               `yaml:"name"`
	Requires       []string             `yaml:"requires"`
	Dependencies   map[string]string    `yaml:"dependencies,omitempty"`
	Actions        map[string]Action    `yaml:"actions,omitempty"`
	Triggers       map[string][]Trigger `yaml:"triggers,omitempty"`
	Values         map[string]any       `yaml:"-"`
	Startup        bool                 `yaml:"startup"`
	Ready          string               `yaml:"ready"`
	Command        []string             `yaml:"command"`
	Directory      string               `yaml:"directory"`
	Env            map[string]string    `yaml:"env"`
	Vars           map[string]string    `yaml:"vars,omitempty"`
	RestartOn      []RestartRule        `yaml:"restart_on,omitempty"`
	SCM            *SCMService          `yaml:"scm,omitempty"`
	Listen         Socket               `yaml:"listen"`
	SocketHandoff  string               `yaml:"socket_handoff,omitempty"`
	SocketMode     *Permissions         `yaml:"socket_mode"`
	MinWorkers     int                  `yaml:"min_workers"`
	MaxWorkers     int                  `yaml:"max_workers"`
	Concurrency    int                  `yaml:"concurrency"`
	IdleTimeout    time.Duration        `yaml:"idle_timeout"`
	StartTimeout   time.Duration        `yaml:"start_timeout"`
	StopTimeout    time.Duration        `yaml:"stop_timeout"`
	RequestTimeout time.Duration        `yaml:"request_timeout"`
	ScaleAt        Percent              `yaml:"scale_at,omitempty"`
	ScaleWindow    time.Duration        `yaml:"scale_window,omitempty"`
	ScaleDelay     time.Duration        `yaml:"scale_delay,omitempty"` // Legacy spelling of scale_window.
	Source         string               `yaml:"-"`
}

// Definitions describe execution; bindings describe when and how it affects a service.
type Action struct {
	Command   []string          `yaml:"command,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	Directory string            `yaml:"directory,omitempty"`
	HTTP      *HTTPRequest      `yaml:"http,omitempty"`
	TCP       *SocketCheck      `yaml:"tcp,omitempty"`
	UDP       *SocketCheck      `yaml:"udp,omitempty"`
	Expect    Expectation       `yaml:"expect,omitempty"`
	Timeout   time.Duration     `yaml:"timeout,omitempty"`
	Retries   int               `yaml:"retries,omitempty"`
	Backoff   time.Duration     `yaml:"backoff,omitempty"`
}

func (app *App) UnmarshalYAML(data []byte) error {
	type fields App
	if err := yaml.Unmarshal(data, (*fields)(app)); err != nil {
		return err
	}
	return yaml.Unmarshal(data, &app.Values)
}

type HTTPRequest struct {
	URL        string            `yaml:"url"`
	Method     string            `yaml:"method,omitempty"`
	Headers    map[string]string `yaml:"headers,omitempty"`
	Body       string            `yaml:"body,omitempty"`
	UnixSocket string            `yaml:"unix_socket,omitempty"`
}

type SocketCheck struct {
	Address string `yaml:"address"`
	Send    string `yaml:"send,omitempty"`
}

type Expectation struct {
	ExitCodes []int  `yaml:"exit_codes,omitempty"`
	Status    []int  `yaml:"status,omitempty"`
	Contains  string `yaml:"contains,omitempty"`
	Regexp    string `yaml:"regexp,omitempty"`
}

type Trigger struct {
	Action           string        `yaml:"action"`
	Scope            string        `yaml:"scope,omitempty"`
	Wait             *bool         `yaml:"wait,omitempty"`
	Interval         time.Duration `yaml:"interval,omitempty"`
	FailureThreshold int           `yaml:"failure_threshold,omitempty"`
	OnFailure        string        `yaml:"on_failure,omitempty"`
}

func (binding Trigger) awaited() bool { return binding.Wait == nil || *binding.Wait }

func (app App) dependencies() map[string]string {
	result := make(map[string]string, len(app.Requires)+len(app.Dependencies))
	for _, name := range app.Requires {
		result[name] = "ready"
	}
	for name, condition := range app.Dependencies {
		result[name] = condition
	}
	return result
}

func (app *App) validateActions() error {
	for name, condition := range app.Dependencies {
		if slices.Contains(app.Requires, name) {
			return fmt.Errorf("dependency %q specified twice", name)
		}
		if condition != "started" && condition != "ready" && condition != "parallel" {
			return fmt.Errorf("dependencies.%s must be started, ready or parallel", name)
		}
	}
	values, err := app.templateValues(app.Listen.Address)
	if err != nil {
		return err
	}
	for name, action := range app.Actions {
		kinds := 0
		for _, present := range []bool{len(action.Command) > 0, action.HTTP != nil, action.TCP != nil, action.UDP != nil} {
			if present {
				kinds++
			}
		}
		if name == "" || kinds != 1 {
			return fmt.Errorf("action %q requires exactly one of command, http, tcp, udp", name)
		}
		if action.Timeout == 0 {
			action.Timeout = 3 * time.Second
		}
		if action.Backoff == 0 {
			action.Backoff = 100 * time.Millisecond
		}
		if action.Timeout <= 0 || action.Backoff < 0 || action.Retries < 0 || action.Retries > 100 {
			return fmt.Errorf("action %q: positive timeout/backoff and 0..100 retries required", name)
		}
		if len(action.Command) == 0 && (len(action.Env) > 0 || action.Directory != "" || len(action.Expect.ExitCodes) > 0) {
			return fmt.Errorf("action %q: env, directory and exit_codes require command", name)
		}
		if action.HTTP == nil && len(action.Expect.Status) > 0 {
			return fmt.Errorf("action %q: status requires http", name)
		}
		if action.UDP != nil && (action.UDP.Send == "" || (action.Expect.Contains == "" && action.Expect.Regexp == "")) {
			return fmt.Errorf("action %q: UDP requires send and a response expectation", name)
		}
		for key := range action.Env {
			if key == "" || strings.ContainsAny(key, "=\x00") {
				return fmt.Errorf("action %q: invalid environment key", name)
			}
		}
		expanded, err := action.expand(values)
		if err != nil {
			return fmt.Errorf("action %q: %w", name, err)
		}
		if expanded.Expect.Regexp != "" {
			if _, err := regexp.Compile(expanded.Expect.Regexp); err != nil {
				return fmt.Errorf("action %q: invalid response regexp", name)
			}
		}
		if expanded.HTTP != nil {
			request, err := http.NewRequest(expanded.HTTP.Method, expanded.HTTP.URL, nil)
			if err != nil || (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.Host == "" {
				return fmt.Errorf("action %q: invalid HTTP request", name)
			}
		}
		for _, check := range []*SocketCheck{expanded.TCP, expanded.UDP} {
			if check != nil {
				if _, _, err := net.SplitHostPort(check.Address); err != nil {
					return fmt.Errorf("action %q: TCP/UDP requires host:port", name)
				}
			}
		}
		for _, status := range action.Expect.Status {
			if status < 100 || status > 599 {
				return fmt.Errorf("action %q: invalid HTTP status", name)
			}
		}
		app.Actions[name] = action
	}
	for event, bindings := range app.Triggers {
		if !slices.Contains([]string{"pre_start", "post_start", "pre_stop", "post_stop", "readiness", "health"}, event) {
			return fmt.Errorf("unknown action trigger %q", event)
		}
		for index := range bindings {
			binding := &bindings[index]
			if _, ok := app.Actions[binding.Action]; !ok {
				return fmt.Errorf("trigger %s: unknown action %q", event, binding.Action)
			}
			if binding.Scope == "" {
				binding.Scope = "worker"
			}
			if binding.Scope != "worker" && binding.Scope != "app" {
				return fmt.Errorf("trigger %s: scope must be worker or app", event)
			}
			periodic := event == "health" || event == "readiness"
			if binding.Interval == 0 && periodic {
				binding.Interval = time.Second
			}
			if binding.FailureThreshold == 0 {
				binding.FailureThreshold = 1
				if periodic {
					binding.FailureThreshold = 3
				}
			}
			if binding.Interval < 0 || (!periodic && binding.Interval != 0) || binding.FailureThreshold < 1 || (!periodic && binding.FailureThreshold != 1) {
				return fmt.Errorf("trigger %s: interval/failure_threshold apply to readiness and health", event)
			}
			if binding.OnFailure == "" {
				binding.OnFailure = "restart"
				if !binding.awaited() || event == "pre_stop" || event == "post_stop" {
					binding.OnFailure = "log"
				}
			}
			if binding.OnFailure != "log" && binding.OnFailure != "restart" {
				return fmt.Errorf("trigger %s: on_failure must be log or restart", event)
			}
			if binding.OnFailure == "restart" && (!binding.awaited() || event == "pre_stop" || event == "post_stop") {
				return fmt.Errorf("trigger %s: restart requires wait and a start/readiness/health trigger", event)
			}
		}
		app.Triggers[event] = bindings
	}
	return nil
}

// Expand every string leaf in an action with the same evaluator as command/env.
func expandFields(value reflect.Value, values map[string]any, field string) error {
	switch value.Kind() {
	case reflect.String:
		expanded, err := expandValue(values, field, value.String())
		if err != nil {
			return err
		}
		value.SetString(expanded)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if err := expandFields(value.Field(index), values, field+"."+value.Type().Field(index).Name); err != nil {
				return err
			}
		}
	case reflect.Pointer:
		if !value.IsNil() {
			clone := reflect.New(value.Type().Elem())
			clone.Elem().Set(value.Elem())
			value.Set(clone)
			return expandFields(value.Elem(), values, field)
		}
	case reflect.Slice:
		clone := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		reflect.Copy(clone, value)
		value.Set(clone)
		for index := 0; index < value.Len(); index++ {
			if err := expandFields(value.Index(index), values, fmt.Sprintf("%s[%d]", field, index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		clone := reflect.MakeMap(value.Type())
		for _, key := range value.MapKeys() {
			expandedKey := reflect.New(key.Type()).Elem()
			expandedKey.Set(key)
			if err := expandFields(expandedKey, values, field+" key"); err != nil {
				return err
			}
			item := reflect.New(value.Type().Elem()).Elem()
			item.Set(value.MapIndex(key))
			if err := expandFields(item, values, field+" value"); err != nil {
				return err
			}
			if clone.MapIndex(expandedKey).IsValid() {
				return fmt.Errorf("duplicate expanded key in %s", field)
			}
			clone.SetMapIndex(expandedKey, item)
		}
		value.Set(clone)
	}
	return nil
}

func (action Action) expand(values map[string]any) (Action, error) {
	err := expandFields(reflect.ValueOf(&action).Elem(), values, "action")
	return action, err
}

const actionOutputLimit = 1 << 20

type actionOutput struct {
	data     []byte
	overflow bool
}

func (output *actionOutput) Write(data []byte) (int, error) {
	count := min(len(data), actionOutputLimit-len(output.data))
	output.data = append(output.data, data[:count]...)
	output.overflow = output.overflow || count < len(data)
	return len(data), nil
}

func (expect Expectation) matches(data []byte) bool {
	if !strings.Contains(string(data), expect.Contains) {
		return false
	}
	if expect.Regexp == "" {
		return true
	}
	matched, err := regexp.Match(expect.Regexp, data)
	return err == nil && matched
}

func (action Action) attempt(ctx context.Context, owner *processOwner, app App, values map[string]any) error {
	if len(action.Command) > 0 {
		cmd := exec.Command(action.Command[0], action.Command[1:]...)
		cmd.Dir = app.Directory
		if action.Directory != "" {
			cmd.Dir = resolve(app.Directory, action.Directory)
		}
		if strings.ContainsAny(action.Command[0], `/\`) {
			cmd.Path = resolve(cmd.Dir, action.Command[0])
		}
		_, environment, err := app.expandCommand(values)
		if err != nil {
			return err
		}
		cmd.Env = os.Environ()
		for key, value := range environment {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		for key, value := range action.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		var output actionOutput
		cmd.Stdout = &output
		cmd.Stderr = io.Discard
		cmd.WaitDelay = 100 * time.Millisecond
		cmd.NewProcessGroup = true
		release, err := app.Identity.apply(cmd)
		if err != nil {
			return fmt.Errorf("action identity unavailable")
		}
		defer release()
		job, err := owner.Start(cmd)
		if err != nil {
			return fmt.Errorf("action command could not start")
		}
		finished := make(chan error, 1)
		go func() { finished <- owner.Wait(job) }()
		select {
		case err = <-finished:
		case <-ctx.Done():
			job.Kill()
			<-finished
			err = ctx.Err()
		}
		if cleanupErr := job.Close(); cleanupErr != nil {
			return fmt.Errorf("action family cleanup: %w", cleanupErr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			return fmt.Errorf("action command did not complete")
		}
		codes := action.Expect.ExitCodes
		if len(codes) == 0 {
			codes = []int{0}
		}
		if !slices.Contains(codes, job.cmd.ProcessState.ExitCode()) {
			return fmt.Errorf("unexpected command exit code %d", job.cmd.ProcessState.ExitCode())
		}
		if output.overflow {
			return fmt.Errorf("action output exceeds 1 MiB")
		}
		if !action.Expect.matches(output.data) {
			return fmt.Errorf("stdout expectation failed")
		}
		return nil
	}
	if action.HTTP != nil {
		check := action.HTTP
		request, err := http.NewRequestWithContext(ctx, check.Method, check.URL, strings.NewReader(check.Body))
		if err != nil {
			return fmt.Errorf("invalid HTTP request")
		}
		for name, value := range check.Headers {
			if strings.EqualFold(name, "Host") {
				request.Host = value
			} else {
				request.Header.Set(name, value)
			}
		}
		transport := &http.Transport{DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		if check.UnixSocket != "" {
			transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", check.UnixSocket)
			}
		}
		client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("HTTP request failed")
		}
		defer response.Body.Close()
		if len(action.Expect.Status) > 0 {
			if !slices.Contains(action.Expect.Status, response.StatusCode) {
				return fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
			}
		} else if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, actionOutputLimit+1))
		if err != nil {
			return fmt.Errorf("HTTP response read failed")
		}
		if len(data) > actionOutputLimit {
			return fmt.Errorf("action response exceeds 1 MiB")
		}
		if !action.Expect.matches(data) {
			return fmt.Errorf("HTTP body expectation failed")
		}
		return nil
	}
	check, network := action.TCP, "tcp"
	if action.UDP != nil {
		check, network = action.UDP, "udp"
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, network, check.Address)
	if err != nil {
		return fmt.Errorf("%s connection failed", network)
	}
	defer connection.Close()
	deadline, _ := ctx.Deadline()
	connection.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	if check.Send != "" {
		if _, err := io.WriteString(connection, check.Send); err != nil {
			return fmt.Errorf("%s send failed", network)
		}
	}
	if action.Expect.Contains == "" && action.Expect.Regexp == "" {
		return nil
	}
	var data []byte
	buffer := make([]byte, 65536)
	for len(data) <= actionOutputLimit {
		count, err := connection.Read(buffer)
		data = append(data, buffer[:count]...)
		if len(data) > actionOutputLimit {
			break
		}
		if action.Expect.matches(data) {
			return nil
		}
		if err != nil || network == "udp" {
			return fmt.Errorf("%s response expectation failed", network)
		}
	}
	return fmt.Errorf("action response exceeds 1 MiB")
}

func (action Action) execute(ctx context.Context, owner *processOwner, app App, values map[string]any) error {
	expanded, err := action.expand(values)
	if err != nil {
		return err
	}
	for attempt := 0; attempt <= action.Retries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		trial, cancel := context.WithTimeout(ctx, action.Timeout)
		err = expanded.attempt(trial, owner, app, values)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < action.Retries {
			timer := time.NewTimer(min(min(action.Backoff, 30*time.Second)*time.Duration(1<<min(attempt, 10)), 30*time.Second))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	return err
}

type SCMService struct {
	Name string   `yaml:"name"`
	Args []string `yaml:"args,omitempty"`
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
	scmServices := make(map[string]string)
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
			if app.SCM == nil && !strings.Contains(app.Command[0], "{{") && strings.ContainsAny(app.Command[0], `/\`) {
				app.Command[0] = resolve(app.Directory, app.Command[0])
			}
			if app.Listen.Network == "unix" {
				app.Listen.Address = resolve(filepath.Dir(file), app.Listen.Address)
			}
			if _, _, err := app.expandLaunch(app.Listen.Address); err != nil {
				return Snapshot{}, fmt.Errorf("%s: %w", file, err)
			}
			if err := app.validateActions(); err != nil {
				return Snapshot{}, fmt.Errorf("%s: %w", file, err)
			}
			if app.SCM != nil {
				name := strings.ToLower(app.SCM.Name)
				if previous, ok := scmServices[name]; ok {
					return Snapshot{}, fmt.Errorf("%s and %s control the same SCM service", previous, file)
				}
				scmServices[name] = file
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
		for dependency := range app.dependencies() {
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
	if app.SCM != nil {
		if runtime.GOOS != "windows" || app.SCM.Name == "" || strings.ContainsRune(app.SCM.Name, 0) {
			return fmt.Errorf("scm requires Windows and a non-empty service name")
		}
		if len(app.Command) != 0 || len(app.Env) != 0 || app.Directory != "" || app.Identity.User != "" || app.Listen != (Socket{}) || app.SocketHandoff != "" {
			return fmt.Errorf("scm uses the registered service command, environment and identity; command, env, directory and socket handoff are unavailable")
		}
		if app.MaxWorkers != 1 || app.Ready != "started" {
			return fmt.Errorf("scm requires max_workers: 1 and uses SCM running status for readiness")
		}
	} else if len(app.Command) == 0 || app.Command[0] == "" {
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
	values, err := app.templateValues(address)
	if err != nil {
		return nil, nil, err
	}
	return app.expandCommand(values)
}

func (app App) templateValues(address string) (map[string]any, error) {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	config := app.Values
	if config == nil {
		data, err := yaml.Marshal(app)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, err
		}
	}
	values := map[string]any{"name": app.Name, "directory": app.Directory, "source": app.Source, "env": environment, "vars": app.Vars, "config": config,
		"runtime": map[string]any{"pid": 0, "workers": 0, "ready_workers": 0, "stopping_workers": 0, "requests": 0, "worker_requests": 0, "ready": false, "telemetry": false, "stopping": false, "pids": []int{}, "exit_code": -1, "started_ns": int64(0), "uptime_ms": int64(0), "event": "", "scope": "worker"}}
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
				return nil, fmt.Errorf("listen.address: %w", err)
			}
			endpoint["host"], endpoint["port"] = host, port
		}
		endpoint["url"] = uri.String()
		values["listen"] = endpoint
	}
	return values, nil
}

// All launch and action interpolation goes through this one evaluator.
func expandValue(values map[string]any, field, value string) (string, error) {
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

func (app App) expandCommand(values map[string]any) ([]string, map[string]string, error) {
	arguments, field := app.Command, "command"
	if app.SCM != nil {
		arguments, field = app.SCM.Args, "scm.args"
	}
	command := make([]string, len(arguments))
	for index, argument := range arguments {
		value, err := expandValue(values, fmt.Sprintf("%s[%d]", field, index), argument)
		if err != nil {
			return nil, nil, err
		}
		command[index] = value
	}
	if app.SCM == nil && (len(command) == 0 || command[0] == "") {
		return nil, nil, fmt.Errorf("expanded command must name an executable")
	}
	if app.SCM == nil && strings.ContainsAny(command[0], `/\`) {
		command[0] = resolve(app.Directory, command[0])
	}
	childEnv := make(map[string]string, len(app.Env))
	for key, value := range app.Env {
		expanded, err := expandValue(values, "env."+key, value)
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

type appCycle struct {
	members  map[*process]struct{}
	scope    *actionScope
	retiring bool
}

type actionScope struct {
	config  App
	address string
	worker  *process
	cycle   *appCycle
	runs    map[string][]*actionRun
}

type actionRun struct {
	scope                             *actionScope
	event                             string
	binding                           Trigger
	running, done, success, cancelled bool
	failures                          int
	next                              time.Time
	cancel                            context.CancelFunc
}

func (service *service) address() string {
	if service.listener != nil {
		return service.listener.Addr().String()
	}
	return service.config.Listen.Address
}

func (scope *actionScope) values(event string, now time.Time) (map[string]any, error) {
	values, err := scope.config.templateValues(scope.address)
	if err != nil {
		return nil, err
	}
	state := values["runtime"].(map[string]any)
	state["event"] = event
	cycle := scope.cycle
	if child := scope.worker; child != nil {
		cycle = child.cycle
		state["pid"], state["exit_code"] = child.pid, child.exitCode
		state["worker_requests"], state["ready"], state["telemetry"], state["stopping"] = len(child.active), child.ready, child.telemetry, !child.stopping.IsZero()
		if !child.pending {
			state["started_ns"], state["uptime_ms"] = child.started.UnixNano(), now.Sub(child.started).Milliseconds()
		}
	} else {
		state["scope"] = "app"
	}
	if cycle != nil {
		workers, ready, stopping, requests := 0, 0, 0, 0
		var pids []int
		for child := range cycle.members {
			if child.exited {
				continue
			}
			if !child.stopping.IsZero() {
				stopping++
			} else {
				workers++
			}
			if child.ready && child.stopping.IsZero() {
				ready++
			}
			requests += len(child.active)
			if child.pid > 0 {
				pids = append(pids, child.pid)
			}
		}
		slices.Sort(pids)
		state["workers"], state["ready_workers"], state["stopping_workers"], state["requests"], state["pids"] = workers, ready, stopping, requests, pids
	}
	return values, nil
}

func (scope *actionScope) cancelChecks() {
	if scope == nil {
		return
	}
	for event, runs := range scope.runs {
		if event == "pre_stop" || event == "post_stop" {
			continue
		}
		for _, run := range runs {
			run.cancelled = true
			if run.cancel != nil {
				run.cancel()
			}
		}
	}
}

func (scope *actionScope) cancelPhase(event string) {
	if scope == nil {
		return
	}
	for _, run := range scope.runs[event] {
		run.cancelled = true
		if run.cancel != nil {
			run.cancel()
		}
	}
}

// Called only from the manager goroutine. Execution happens off-loop and
// returns an immutable result; it never mutates a process or configuration.
func (manager *manager) phase(scope *actionScope, event string, now time.Time) bool {
	if scope == nil {
		return true
	}
	runs, exists := scope.runs[event]
	if !exists {
		kind := "app"
		if scope.worker != nil {
			kind = "worker"
		}
		for _, binding := range scope.config.Triggers[event] {
			if binding.Scope == kind {
				runs = append(runs, &actionRun{scope: scope, event: event, binding: binding})
			}
		}
		scope.runs[event] = runs
	}
	ok := true
	for _, run := range runs {
		if !run.running && !run.cancelled && !now.Before(run.next) && (!run.done || event == "health" || (event == "readiness" && !run.success)) {
			values, err := scope.values(event, now)
			run.running = true
			ctx, cancel := context.WithCancel(context.Background())
			run.cancel = cancel
			manager.actionTasks++
			go func() {
				defer cancel()
				result := err
				if result == nil {
					result = scope.config.Actions[run.binding.Action].execute(ctx, manager.owner, scope.config, values)
				}
				manager.messages <- message{action: run, err: result}
			}()
		}
		if run.binding.awaited() {
			passed := run.done && run.success
			if event == "health" {
				passed = run.failures < run.binding.FailureThreshold
			}
			if event != "readiness" && event != "health" && run.done && run.binding.OnFailure == "log" {
				passed = true
			}
			ok = ok && passed
		}
	}
	return ok
}

func (manager *manager) actionResult(run *actionRun, err error, now time.Time) {
	manager.actionTasks--
	run.running = false
	run.cancel = nil
	if run.cancelled {
		return
	}
	run.done, run.success = true, err == nil
	run.next = now.Add(run.binding.Interval)
	if err == nil {
		run.failures = 0
	} else {
		run.failures++
	}
	manager.log.Debug("action completed", "app", run.scope.config.Name, "event", run.event, "action", run.binding.Action, "scope", run.binding.Scope, "success", err == nil)
	if err == nil {
		return
	}
	manager.log.Warn("action failed", "app", run.scope.config.Name, "event", run.event, "action", run.binding.Action, "failures", run.failures, "error", err)
	if !run.binding.awaited() || run.binding.OnFailure != "restart" || run.failures < run.binding.FailureThreshold {
		return
	}
	stop := func(child *process) {
		if child.stopping.IsZero() && !child.exited {
			child.failed = true
			manager.stop(child, now, "action failed: "+run.binding.Action)
		}
	}
	if run.scope.worker != nil {
		stop(run.scope.worker)
	} else {
		for child := range run.scope.cycle.members {
			stop(child)
		}
	}
}

func (manager *manager) lifecycle(child *process, now time.Time) {
	cycle := child.cycle
	if cycle == nil {
		return
	} // Tests may construct a process without action state.
	if child.exited {
		child.scope.cancelPhase("pre_stop")
		workerDone := manager.phase(child.scope, "post_stop", now)
		allExited := true
		for other := range cycle.members {
			allExited = allExited && other.exited
		}
		appDone := true
		if allExited {
			cycle.scope.cancelPhase("pre_stop")
			appDone = manager.phase(cycle.scope, "post_stop", now)
		}
		if workerDone && appDone {
			delete(manager.workers, child)
			delete(child.service.workers, child)
			delete(cycle.members, child)
		}
		return
	}
	if !child.stopping.IsZero() {
		workerDone := manager.phase(child.scope, "pre_stop", now)
		appDone := true
		if cycle.retiring {
			appDone = manager.phase(cycle.scope, "pre_stop", now)
		}
		if !child.stopSent && (workerDone && appDone || now.Sub(child.stopping) >= child.config.StopTimeout) {
			child.stopSent = true
			if child.pending {
				child.exited = true
			} else {
				manager.signalStop(child, now)
			}
		}
		return
	}
	if child.pending {
		return
	}
	if child.scm != nil && !child.baseReady {
		return
	}
	postWorker := manager.phase(child.scope, "post_start", now)
	postApp := manager.phase(cycle.scope, "post_start", now)
	if !child.baseReady || !postWorker || !postApp {
		child.ready = false
		return
	}
	readyWorker := manager.phase(child.scope, "readiness", now)
	readyApp := manager.phase(cycle.scope, "readiness", now)
	if !readyWorker || !readyApp {
		child.ready = false
		return
	}
	// Capture checks against the published state, then apply their new outcome.
	// Clearing ready first would expose a false not-ready state to every probe.
	healthWorker := manager.phase(child.scope, "health", now)
	healthApp := manager.phase(cycle.scope, "health", now)
	child.ready = healthWorker && healthApp && manager.dependenciesReady(child.config, true)
	child.wasReady = child.wasReady || child.ready
}

func (manager *manager) dependenciesReady(app App, readiness bool) bool {
	for name, condition := range app.dependencies() {
		if condition == "parallel" && !readiness {
			continue
		}
		found := false
		if dependency := manager.services[name]; dependency != nil {
			for child := range dependency.workers {
				if child.pending || child.exited || !child.stopping.IsZero() {
					continue
				}
				if condition == "started" || child.ready {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type process struct {
	wasReady  bool
	pending   bool
	exited    bool
	baseReady bool
	stopSent  bool
	exitCode  int
	scope     *actionScope
	cycle     *appCycle
	pid       int
	scm       serviceControl
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

type serviceControl interface {
	Stop()
	Kill() error
}

type message struct {
	action     *actionRun
	scm        bool
	pid        int
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
	cycle := service.cycle
	if cycle == nil || cycle.retiring {
		cycle = &appCycle{members: make(map[*process]struct{})}
		cycle.scope = &actionScope{config: service.config, address: service.address(), cycle: cycle, runs: make(map[string][]*actionRun)}
		service.cycle = cycle
	}
	child := &process{pending: true, exitCode: -1, service: service, config: service.config, cycle: cycle, started: now, active: make(map[string]time.Time)}
	child.scope = &actionScope{config: child.config, address: service.address(), worker: child, runs: make(map[string][]*actionRun)}
	cycle.members[child] = struct{}{}
	manager.workers[child] = struct{}{}
	service.workers[child] = struct{}{}
	appOK := manager.phase(cycle.scope, "pre_start", now)
	workerOK := manager.phase(child.scope, "pre_start", now)
	if appOK && workerOK {
		if err := manager.startProcess(child, now); err != nil {
			child.scope.cancelChecks()
			delete(manager.workers, child)
			delete(service.workers, child)
			delete(cycle.members, child)
			if len(cycle.members) == 0 {
				cycle.retiring = true
				cycle.scope.cancelChecks()
			}
			return err
		}
		manager.lifecycle(child, now)
	}
	return nil
}

func (manager *manager) startProcess(child *process, now time.Time) error {
	service := child.service
	if service.config.SCM != nil {
		return manager.spawnSCM(child, now)
	}
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
	values, err := child.scope.values("launch", now)
	if err != nil {
		return err
	}
	command, environment, err := app.expandCommand(values)
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
	child.pid, child.cmd, child.control, child.job = cmd.Process.Pid, cmd, control, job
	child.pending = false
	control = nil
	if app.Ready == "started" {
		child.baseReady = true
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
	cycle         *appCycle
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
	actionTasks    int
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
	return run(ctx, path, logger, nil)
}

func run(ctx context.Context, path string, logger *slog.Logger, ready func()) error {
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
	if ready != nil {
		ready()
	}
	for {
		if manager.fatal != nil && result == nil {
			result = manager.fatal
		}
		if manager.fatal != nil && !stopping {
			stopping = true
			cancelWatch()
			manager.shutdown()
		}
		if stopping && len(manager.workers) == 0 && manager.actionTasks == 0 {
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
	child.ready = false
	child.scope.cancelChecks()
	if child.cycle != nil {
		retiring := true
		for other := range child.cycle.members {
			if other.stopping.IsZero() && !other.exited {
				retiring = false
			}
		}
		if retiring {
			child.cycle.retiring = true
			child.cycle.scope.cancelChecks()
		}
	}
	if child.failed {
		// Replacements can start before this process exits; throttle failures now.
		child.service.demand = true
		child.service.backoff(now)
	}
	manager.log.Info("worker stopping", "app", child.service.name, "pid", child.pid, "reason", reason)
	if child.scope != nil {
		manager.lifecycle(child, now)
		return
	}
	manager.signalStop(child, now)
}

func (manager *manager) signalStop(child *process, now time.Time) {
	if child.scm != nil {
		child.scm.Stop()
		return
	}
	if child.telemetry && child.control != nil {
		// One short write per process fits in the empty stdin pipe; no reader
		// cooperation is needed to return. The existing deadline bounds draining.
		if _, err := fmt.Fprintf(child.control, "v=1 event=stop ts=%d\n", now.UnixNano()); err == nil {
			return
		} else {
			manager.log.Warn("stdin stop failed; sending signal", "pid", child.pid, "error", err)
		}
	}
	if err := child.cmd.Process.Signal(StopSignal); err != nil && !errors.Is(err, os.ErrProcessDone) {
		manager.log.Error("graceful signal failed", "pid", child.pid, "error", err)
	}
}

func (manager *manager) handle(msg message, now time.Time) {
	if msg.action != nil {
		manager.actionResult(msg.action, msg.err, now)
		return
	}
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
	if msg.scm {
		child.pid = msg.pid
		child.baseReady = msg.ready
		if child.cycle == nil {
			child.ready = msg.ready
		}
		manager.lifecycle(child, now)
		child.idleSince = now
		manager.log.Info("SCM service status", "app", service.name, "pid", msg.pid, "ready", msg.ready)
		return
	}
	if msg.exited {
		if msg.cleanupErr != nil {
			manager.fatal = fmt.Errorf("clean up worker %d: %w", child.pid, msg.cleanupErr)
		}
		child.exited = true
		child.ready = false
		child.scope.cancelChecks()
		if child.cmd != nil && child.cmd.ProcessState != nil {
			child.exitCode = child.cmd.ProcessState.ExitCode()
		}
		manager.log.Info("worker exited", "app", service.name, "pid", child.pid, "error", msg.err)
		if child.stopping.IsZero() {
			service.demand = true
			service.backoff(now)
		}
		if child.cycle != nil {
			active := false
			for other := range child.cycle.members {
				if !other.exited && other.stopping.IsZero() {
					active = true
				}
			}
			if !active {
				child.cycle.retiring = true
				child.cycle.scope.cancelChecks()
			}
			manager.lifecycle(child, now)
		} else {
			delete(manager.workers, child)
			delete(service.workers, child)
		}
		return
	}
	if msg.probe {
		child.probing = false
		child.nextProbe = now.Add(100 * time.Millisecond)
		if msg.ready {
			child.baseReady = true
			child.idleSince = now
		}
		return
	}
	if msg.handshake {
		child.telemetry = msg.ready
		if msg.ready {
			if child.config.Ready == "event" {
				child.baseReady = true
			}
			child.idleSince = now
		} else if child.config.Ready == "event" {
			child.failed = true
			manager.stop(child, now, "missing ready handshake")
		}
		manager.log.Info("worker stdout detected", "app", service.name, "pid", child.pid, "telemetry", child.telemetry)
		return
	}
	if msg.err != nil {
		if child.stopping.IsZero() {
			child.failed = true
			manager.log.Warn("worker event stream failed", "pid", child.pid, "error", msg.err)
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
		manager.log.Debug("request started", "app", service.name, "pid", child.pid, "id", event.ID, "ts", event.Time)
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
		manager.log.Debug("request finished", "app", service.name, "pid", child.pid,
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
		manager.lifecycle(child, now)
		if child.exited {
			continue
		}
		if !child.stopping.IsZero() {
			if !child.pending && !child.killed && now.Sub(child.stopping) >= app.StopTimeout {
				child.killed = true
				manager.log.Warn("worker exceeded graceful timeout", "pid", child.pid)
				var err error
				if child.scm != nil {
					err = child.scm.Kill()
				} else {
					err = child.job.Kill()
				}
				if err != nil && !errors.Is(err, os.ErrProcessDone) {
					manager.log.Error("worker kill failed", "error", err)
				}
			}
			continue
		}
		if !child.wasReady && !child.ready && now.Sub(child.started) >= app.StartTimeout {
			child.failed = true
			manager.stop(child, now, "startup timeout")
		}
		if !child.pending && child.scm == nil && !child.baseReady && app.Ready != "event" && app.Ready != "started" && !child.probing && !now.Before(child.nextProbe) && child.stopping.IsZero() {
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
				if _, depends := dependent.config.dependencies()[child.service.name]; depends {
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
		for dependency := range current.config.dependencies() {
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
		if !manager.dependenciesReady(app, false) {
			if len(service.workers) > 0 {
				service.demand = true
			}
			for child := range service.workers {
				manager.stop(child, now, "dependency unavailable")
			}
			continue
		}
		for child := range service.workers {
			if !child.pending || !child.stopping.IsZero() || child.exited {
				continue
			}
			appOK := manager.phase(child.cycle.scope, "pre_start", now)
			workerOK := manager.phase(child.scope, "pre_start", now)
			if appOK && workerOK {
				if err := manager.startProcess(child, now); err != nil {
					manager.log.Error("worker start failed", "app", service.name, "error", err)
					child.failed = true
					manager.stop(child, now, "start failed")
				} else {
					manager.lifecycle(child, now)
				}
			}
		}
		service.observePressure(now)
		pressureGrowth := service.pressure.evaluate(now, app.ScaleWindow, app.ScaleAt)
		available, starting := 0, 0
		for child := range service.workers {
			if !child.stopping.IsZero() || child.exited {
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
		if app.SCM != nil && len(service.workers) != 0 {
			grow = false // One SCM name is one instance, including while stopping.
		}
		if grow && (starting == 0 || available < app.MinWorkers) && available < app.MaxWorkers && !now.Before(service.retryAt) {
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
