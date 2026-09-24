package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestSCMHostGraceful(t *testing.T) {
	requests := make(chan svc.ChangeRequest, 2)
	statuses := make(chan svc.Status, 8)
	finished := make(chan uint32, 1)
	drain := make(chan struct{})
	host := &scmHost{run: func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		<-drain
		return nil
	}}
	go func() { _, code := host.Execute(nil, requests, statuses); finished <- code }()
	expect := func(state svc.State) {
		t.Helper()
		select {
		case status := <-statuses:
			if status.State != state {
				t.Fatalf("SCM state %v, want %v", status.State, state)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("SCM status timeout")
		}
	}
	expect(svc.StartPending)
	expect(svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
	expect(svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	expect(svc.StopPending)
	select {
	case <-finished:
		t.Fatal("SCM host exited before workers drained")
	default:
	}
	close(drain)
	select {
	case code := <-finished:
		if code != 0 || host.err != nil {
			t.Fatalf("SCM host exit: %d %v", code, host.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SCM host did not complete shutdown")
	}
}

func TestSCMHostStartupError(t *testing.T) {
	failure := errors.New("configuration failed")
	host := &scmHost{run: func(context.Context, func()) error { return failure }}
	statuses := make(chan svc.Status, 3)
	specific, code := host.Execute(nil, make(chan svc.ChangeRequest), statuses)
	if !specific || code == 0 || !errors.Is(host.err, failure) || len(statuses) != 1 {
		t.Fatal("startup failure was reported as a running/successful service")
	}
}

func TestSCMNativeSubscription(t *testing.T) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseServiceHandle(handle)
	name, _ := windows.UTF16PtrFromString("EventLog")
	service, err := windows.OpenService(handle, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseServiceHandle(service)
	unsubscribe, err := subscribeSCM(service, make(chan struct{}, 1))
	if err != nil {
		t.Fatal(err)
	}
	unsubscribe() // Read-only: do not control an existing machine service.
	if _, err := openSCM(fmt.Sprintf("ooth-missing-%d", os.Getpid())); !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.Fatalf("missing service: %v", err)
	}
}

func TestSCMProcessHandleTermination(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerHelper$")
	cmd.Env = append(os.Environ(), "TEST_OOTH_WORKER=1", "TEST_OOTH_VIRTUAL=1")
	cmd.NewProcessGroup = true
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := terminateSCMProcess(handle); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(handle, 3000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("forced process did not exit: %v %v", state, err)
	}
	if err := terminateSCMProcess(handle); err != nil {
		t.Fatal("already exited retained handle:", err)
	}
}

func TestSCMPendingPIDIsNotTrusted(t *testing.T) {
	control := &scmControl{}
	for _, state := range []svc.State{svc.StartPending, svc.StopPending, svc.Stopped} {
		// Even a nonzero PID in these states is not valid according to SCM.
		if err := control.capture(svc.Status{State: state, ProcessId: uint32(os.Getpid())}); err != nil || control.process != 0 {
			t.Fatalf("captured a pending/stopped PID: %v", err)
		}
	}
}

// Run from an elevated test session to exercise the actual SCM dispatcher and
// controller together. The temporary service is always removed after the test.
func TestSCMLifecycle(t *testing.T) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_CREATE_SERVICE)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skip("native SCM lifecycle requires permission to create a temporary Windows service")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseServiceHandle(handle)
	directory := t.TempDir()
	path := filepath.Join(directory, "empty.yaml")
	write(t, path, "watch: [missing-app-*.yaml]\n")
	name := fmt.Sprintf("ooth-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	registered, err := (&mgr.Mgr{Handle: handle}).CreateService(name, os.Args[0], mgr.Config{StartType: mgr.StartManual}, "-test.run=^TestSCMHelper$", "--", name, path)
	if err != nil {
		t.Fatal(err)
	}
	defer registered.Close()
	defer func() {
		if err := registered.Delete(); err != nil {
			t.Error(err)
		}
	}()
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprint(force), func(t *testing.T) {
			control, err := openSCM(name)
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				err, cleanupErr := control.run(nil, func(status svc.Status) {
					if status.State == svc.Running {
						select {
						case ready <- struct{}{}:
						default:
						}
					}
				}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				done <- errors.Join(err, cleanupErr)
				close(done)
			}()
			t.Cleanup(func() {
				control.Stop()
				control.Kill()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Error("temporary SCM service cleanup timed out")
				}
			})
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("SCM startup: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("SCM startup timed out")
			}
			if force {
				if err := control.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				control.Stop()
			}
			select {
			case err := <-done:
				if !force && err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SCM stop timed out")
			}
		})
	}
}

func TestSCMHelper(t *testing.T) {
	for index, argument := range os.Args {
		if argument == "--" && index+2 < len(os.Args) {
			if err := runSCM(os.Args[index+1], os.Args[index+2], slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
				os.Exit(2)
			}
			return
		}
	}
}
