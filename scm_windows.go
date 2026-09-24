package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type scmHost struct {
	run func(context.Context, func()) error
	err error
}

func runSCM(name, path string, logger *slog.Logger) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("-service requires an absolute -config path")
	}
	host := &scmHost{run: func(ctx context.Context, ready func()) error {
		return run(ctx, path, logger, ready)
	}}
	if err := svc.Run(name, host); err != nil {
		return err
	}
	return host.err
}

func (host *scmHost) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	status := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 10000}
	statuses <- status
	go func() { done <- host.run(ctx, func() { close(ready) }) }()
	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ready:
			ready = nil
			if ctx.Err() == nil {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- status
			}
		case request := <-requests:
			switch request.Cmd {
			case svc.Stop, svc.Shutdown:
				if ctx.Err() == nil {
					status = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 10000}
					statuses <- status
					cancel()
				}
			case svc.Interrogate:
				statuses <- status
			}
		case <-heartbeat.C:
			if status.State == svc.StartPending || status.State == svc.StopPending {
				status.CheckPoint++
				statuses <- status
			}
		case host.err = <-done:
			if host.err != nil {
				return true, 1
			}
			return false, 0
		}
	}
}

// Native subscriptions wake the controller; no periodic service/process scan.
// Integer callback tokens keep Go pointers out of memory retained by Windows.
var scmSubscriptions sync.Map
var scmSubscriptionID atomic.Uint64
var scmCallback = windows.NewCallback(func(_ uint32, token uintptr) uintptr {
	if value, ok := scmSubscriptions.Load(token); ok {
		select {
		case value.(chan struct{}) <- struct{}{}:
		default:
		}
	}
	return 0
})

func subscribeSCM(handle windows.Handle, wake chan struct{}) (func(), error) {
	token := uintptr(scmSubscriptionID.Add(1))
	scmSubscriptions.Store(token, wake)
	var subscription uintptr
	if err := windows.SubscribeServiceChangeNotifications(handle, windows.SC_EVENT_STATUS_CHANGE, scmCallback, token, &subscription); err != nil {
		scmSubscriptions.Delete(token)
		return nil, err
	}
	return func() {
		// This waits for in-flight callbacks before releasing their context.
		windows.UnsubscribeServiceChangeNotifications(subscription)
		scmSubscriptions.Delete(token)
	}, nil
}

type scmControl struct {
	service *mgr.Service
	wake    chan struct{}
	mu      sync.Mutex
	process windows.Handle
	pid     uint32
	stop    bool
	force   bool
}

func openSCM(name string) (*scmControl, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(manager)
	serviceName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, err := windows.OpenService(manager, serviceName, windows.SERVICE_START|windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return nil, err
	}
	service := &mgr.Service{Name: name, Handle: handle}
	config, err := service.Config()
	if err != nil {
		service.Close()
		return nil, err
	}
	if config.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		service.Close()
		return nil, fmt.Errorf("SCM service %q must have its own process for safe timeout termination", name)
	}
	return &scmControl{service: service, wake: make(chan struct{}, 1)}, nil
}

func (control *scmControl) wakeUp() {
	select {
	case control.wake <- struct{}{}:
	default:
	}
}

func (control *scmControl) Stop() {
	control.mu.Lock()
	control.stop = true
	control.mu.Unlock()
	control.wakeUp()
}

func (control *scmControl) Kill() error {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.force = true
	control.wakeUp()
	if control.process == 0 {
		return nil // Start-pending: apply it as soon as SCM publishes a PID.
	}
	return terminateSCMProcess(control.process)
}

func terminateSCMProcess(handle windows.Handle) error {
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || state == windows.WAIT_OBJECT_0 {
		return err
	}
	return windows.TerminateProcess(handle, 1)
}

func scmPID(status svc.Status) uint32 {
	switch status.State {
	case svc.Running, svc.PausePending, svc.Paused, svc.ContinuePending:
		return status.ProcessId
	default:
		return 0 // SCM does not guarantee a valid PID in start/stop-pending.
	}
}

// Retain the process handle so a later kill cannot target a reused PID. Only
// one incarnation belongs to this controller; another SCM recovery is external.
func (control *scmControl) capture(status svc.Status) error {
	if scmPID(status) == 0 {
		return nil
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.pid != 0 {
		if control.pid != status.ProcessId {
			return fmt.Errorf("SCM service %q was restarted externally", control.service.Name)
		}
		return nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, status.ProcessId)
	if err != nil {
		return err
	}
	current, err := control.service.Query()
	if err != nil || scmPID(current) != status.ProcessId {
		windows.CloseHandle(handle)
		return err
	}
	control.pid, control.process = status.ProcessId, handle
	if control.force {
		return terminateSCMProcess(handle)
	}
	return nil
}

func (control *scmControl) run(args []string, report func(svc.Status), logger *slog.Logger) (result error, cleanupErr error) {
	owned, stopped := false, false
	defer func() {
		control.mu.Lock()
		defer control.mu.Unlock()
		if owned && !stopped {
			// A monitoring error must not leave an untracked live service.
			if control.process == 0 {
				_, cleanupErr = control.service.Control(svc.Stop)
				if cleanupErr == nil {
					cleanupErr = fmt.Errorf("SCM stop requested but termination could not be confirmed")
				}
			} else {
				cleanupErr = terminateSCMProcess(control.process)
			}
		}
		if control.process != 0 {
			windows.CloseHandle(control.process)
			control.process = 0
		}
		control.service.Close()
	}()
	unsubscribe, err := subscribeSCM(control.service.Handle, control.wake)
	if err != nil {
		return err, nil
	}
	defer unsubscribe()
	status, err := control.service.Query()
	if err != nil {
		return err, nil
	}
	if status.State != svc.Stopped {
		return fmt.Errorf("SCM service %q is already active; refusing to take ownership", control.service.Name), nil
	}
	if err := control.service.Start(args...); err != nil {
		return err, nil
	}
	owned = true
	stopSent := false
	var previous svc.Status
	for {
		status, err = control.service.Query()
		if err != nil {
			return err, nil
		}
		if status.State == svc.Stopped {
			stopped = true
			if status.Win32ExitCode != 0 || status.ServiceSpecificExitCode != 0 {
				return fmt.Errorf("SCM exit codes: Windows=%d service=%d", status.Win32ExitCode, status.ServiceSpecificExitCode), nil
			}
			return nil, nil
		}
		if err := control.capture(status); err != nil {
			return err, nil
		}
		control.mu.Lock()
		status.ProcessId = control.pid
		control.mu.Unlock()
		if status != previous {
			report(status)
			previous = status
		}
		control.mu.Lock()
		stop := control.stop
		control.mu.Unlock()
		if stop && !stopSent && (status.State == svc.Running || status.State == svc.Paused) {
			stopSent = true
			if _, err := control.service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				logger.Warn("SCM graceful stop failed; timeout still applies", "service", control.service.Name, "error", err)
			}
		}
		<-control.wake
	}
}

func (manager *manager) spawnSCM(service *service, now time.Time) error {
	app := service.config
	args, _, err := app.expandLaunch("")
	if err != nil {
		return err
	}
	control, err := openSCM(app.SCM.Name)
	if err != nil {
		return fmt.Errorf("SCM %q: %w", app.SCM.Name, err)
	}
	child := &process{scm: control, service: service, config: app, started: now}
	manager.workers[child] = struct{}{}
	service.workers[child] = struct{}{}
	service.demand = false
	manager.log.Info("worker started", "app", service.name, "pid", 0, "scm", app.SCM.Name)
	go func() {
		err, cleanupErr := control.run(args, func(status svc.Status) {
			manager.messages <- message{process: child, scm: true, pid: int(status.ProcessId), ready: status.State == svc.Running}
		}, manager.log)
		manager.messages <- message{process: child, exited: true, err: err, cleanupErr: cleanupErr}
	}()
	return nil
}
