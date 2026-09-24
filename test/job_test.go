package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
)

// An intermediate parent exits immediately after spawning a grandchild. No
// lifetime relationship is reconstructed by the supervisor or by this fixture.
func TestProcessTreeHelper(t *testing.T) {
	stage := os.Getenv("TEST_TREE_STAGE")
	if stage == "" {
		return
	}
	directory := os.Getenv("TEST_TREE_DIRECTORY")
	if stage != "leaf" {
		next := "middle"
		if stage == "middle" {
			next = "leaf"
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
		cmd.Env = append(os.Environ(), "TEST_TREE_STAGE="+next)
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if stage == "root" && os.Getenv("TEST_TREE_WAIT_LEAF") == "1" {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(directory, "leaf")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(4)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, stage), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(3)
	}
	if stage == "middle" || (stage == "root" && os.Getenv("TEST_TREE_EXIT") == "1") {
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func treeCommand(t *testing.T, directory string, exit bool) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), "TEST_TREE_STAGE=root", "TEST_TREE_DIRECTORY="+directory)
	if exit {
		cmd.Env = append(cmd.Env, "TEST_TREE_EXIT=1")
	}
	cmd.NewProcessGroup = true
	cmd.Stderr = os.Stderr
	return cmd
}

func treePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing process marker: %s", path)
	return 0
}

func TestProcessJobOwnsOrphans(t *testing.T) {
	for _, rootExits := range []bool{true, false} {
		t.Run(strconv.FormatBool(rootExits), func(t *testing.T) {
			owner, err := newProcessOwner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if !treeOwnershipAvailable(owner) {
				if os.Getenv("OOTH_CGROUP_ROOT") != "" {
					t.Fatal("requested test cgroup is unavailable")
				}
				t.Skip("set OOTH_CGROUP_ROOT to a writable delegated cgroup v2")
			}
			directory := t.TempDir()
			cmd := treeCommand(t, directory, rootExits)
			job, err := owner.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer job.Close()
			waited := false
			defer func() {
				job.Kill()
				if !waited {
					owner.Wait(job)
				}
			}()
			if rootExits {
				if err := owner.Wait(job); err != nil {
					t.Fatal(err)
				}
				waited = true
			}
			leaf := treePID(t, filepath.Join(directory, "leaf"))
			middle := treePID(t, filepath.Join(directory, "middle"))
			members, err := job.Members()
			if err != nil || !slices.Contains(members, leaf) {
				t.Fatalf("orphan lost from kernel group: leaf=%d members=%v error=%v", leaf, members, err)
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			if !waited {
				owner.Wait(job)
				waited = true
			}
			assertProcessGone(t, leaf)
			assertProcessGone(t, middle)
		})
	}
}

func TestSupervisorCrashCleansDescendants(t *testing.T) {
	// Every root crashes immediately, leaving a grandchild. Verify that the
	// supervisor's normal restart path cleans up each old family.
	if runtime.GOOS == "linux" && os.Getenv("OOTH_CGROUP_ROOT") == "" {
		t.Skip("set OOTH_CGROUP_ROOT to test cgroup containment")
	}
	owner, err := newProcessOwner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	available := treeOwnershipAvailable(owner)
	owner.Close()
	if !available {
		t.Fatal("requested test cgroup is unavailable")
	}
	directory := t.TempDir()
	cmd := treeCommand(t, directory, true)
	app := App{Name: "tree", Command: cmd.Args, Startup: true, Ready: "started", MaxWorkers: 1,
		Env:         map[string]string{"TEST_TREE_STAGE": "root", "TEST_TREE_DIRECTORY": directory, "TEST_TREE_EXIT": "1", "TEST_TREE_WAIT_LEAF": "1"},
		Concurrency: 1, IdleTimeout: time.Second, StartTimeout: time.Second, StopTimeout: 100 * time.Millisecond, ScaleDelay: time.Millisecond}
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['app/ooth.yaml']\n")
	writeApp(t, filepath.Join(directory, "app", "ooth.yaml"), app)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("supervisor tree shutdown timed out")
		}
	})
	root := treePID(t, filepath.Join(directory, "root"))
	leaf := treePID(t, filepath.Join(directory, "leaf"))
	assertProcessGone(t, root)
	assertProcessGone(t, leaf)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if next := treePID(t, filepath.Join(directory, "root")); next != root {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("crashed root was not restarted")
}
