package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

// Test-only SCM client: production ooth implements just the service host.
// Run elevated to register a temporary host and verify its ordinary child drains.
func TestSCMHostLifecycle(t *testing.T) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_CREATE_SERVICE)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skip("native SCM host test requires permission to create a temporary Windows service")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseServiceHandle(handle)
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	marker := filepath.Join(directory, "worker")
	write(t, path, "watch: [app.yaml]\n")
	app := restartApp("")
	app.RestartOn = nil
	app.Command = []string{os.Args[0], "-test.run=^TestSCMWorkerHelper$"}
	app.Env = map[string]string{"TEST_SCM_MARKER": marker}
	writeApp(t, filepath.Join(directory, "app.yaml"), app)
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
	var process windows.Handle
	defer func() {
		if process != 0 {
			// Failure cleanup is limited to the retained temporary host handle.
			if state, _ := windows.WaitForSingleObject(process, 0); state != windows.WAIT_OBJECT_0 {
				windows.TerminateProcess(process, 1)
				windows.WaitForSingleObject(process, 5000)
			}
			windows.CloseHandle(process)
		}
	}()
	waitState := func(expected svc.State) svc.Status {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			status, err := registered.Query()
			if err != nil {
				t.Fatal(err)
			}
			if status.State == expected {
				return status
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("SCM host did not reach state %v", expected)
		return svc.Status{}
	}
	if err := registered.Start(); err != nil {
		t.Fatal(err)
	}
	status := waitState(svc.Running)
	process, err = windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, status.ProcessId)
	if err != nil {
		t.Fatal(err)
	}
	var workerPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil {
			workerPID, _ = strconv.Atoi(string(data))
			if workerPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if workerPID == 0 {
		t.Fatal("SCM host did not start its ordinary worker")
	}
	worker, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(workerPID))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(worker)
	if _, err := registered.Control(svc.Stop); err != nil {
		t.Fatal(err)
	}
	status = waitState(svc.Stopped)
	if status.Win32ExitCode != 0 || status.ServiceSpecificExitCode != 0 {
		t.Fatalf("SCM host exit status: %v", status)
	}
	if _, err := os.Stat(marker + ".stopped"); err != nil {
		t.Fatal("SCM host did not gracefully stop its worker:", err)
	}
	if state, err := windows.WaitForSingleObject(worker, 0); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatal("SCM host stopped before its worker exited")
	}
	if state, err := windows.WaitForSingleObject(process, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatal("SCM host did not exit")
	}
}

func TestSCMWorkerHelper(t *testing.T) {
	marker := os.Getenv("TEST_SCM_MARKER")
	if marker == "" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	testWorkerControl(cancel)
	if err := WriteEvent(os.Stdout, Event{Type: "ready", Time: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	write(t, marker, strconv.Itoa(os.Getpid()))
	<-ctx.Done()
	write(t, marker+".stopped", "drained")
	os.Exit(0)
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
