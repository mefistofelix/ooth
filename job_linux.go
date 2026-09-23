package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

type processOwner struct {
	mu                sync.Mutex
	root              string
	managed           map[int]*exec.Cmd
	log               *slog.Logger
	signals           chan os.Signal
	done              chan struct{}
	previousSubreaper int32
	groupUnavailable  bool
}

func newProcessOwner(root string, logger *slog.Logger) (*processOwner, error) {
	owner := &processOwner{managed: make(map[int]*exec.Cmd), log: logger}
	if root == "" {
		root = os.Getenv("OOTH_CGROUP_ROOT")
	}
	if root == "" {
		data, err := os.ReadFile("/proc/self/cgroup")
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if name, ok := strings.CutPrefix(line, "0::/"); ok {
					root = filepath.Join("/sys/fs/cgroup", name)
				}
			}
		}
	}
	var err error
	if root == "" {
		err = fmt.Errorf("cgroup v2 is not mounted at /sys/fs/cgroup; configure cgroup explicitly")
	} else {
		owner.root, err = os.MkdirTemp(root, "ooth-")
		if err == nil {
			var file *os.File
			file, err = os.OpenFile(filepath.Join(owner.root, "cgroup.kill"), os.O_WRONLY, 0)
			if err == nil {
				file.Close()
			}
		}
	}
	if err != nil {
		if owner.root != "" {
			os.Remove(owner.root)
			owner.root = ""
		}
		logger.Warn("descendant supervision disabled; monitoring direct children only", "error", err)
	}
	if owner.root != "" {
		// PR_SET_CHILD_SUBREAPER: cgroups keep membership; this adopts orphans.
		if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 37, uintptr(unsafe.Pointer(&owner.previousSubreaper)), 0, 0, 0, 0); err != 0 {
			os.Remove(owner.root)
			return nil, fmt.Errorf("read child subreaper: %w", err)
		}
		if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); err != 0 {
			os.Remove(owner.root)
			return nil, fmt.Errorf("enable child subreaper: %w", err)
		}
	}
	if owner.root != "" || os.Getpid() == 1 {
		owner.signals = make(chan os.Signal, 1)
		owner.done = make(chan struct{})
		signal.Notify(owner.signals, syscall.SIGCHLD)
		go func() {
			defer close(owner.done)
			for range owner.signals {
				owner.mu.Lock()
				owner.reap()
				owner.mu.Unlock()
			}
		}()
	}
	return owner, nil
}

func (owner *processOwner) Start(cmd *exec.Cmd) (*processJob, error) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	root := owner.root
	if owner.groupUnavailable {
		root = ""
	}
	job, err := newProcessJob(cmd, root)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		if cleanupErr := job.Close(); cleanupErr != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		// A writable hierarchy can still be unusable for clone3: for example a
		// container's seccomp policy, or no migration permission from the parent.
		// Only downgrade after the same command succeeds without cgroup placement.
		if root == "" || (!errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EOPNOTSUPP)) {
			return nil, err
		}
		cmd.SysProcAttr.UseCgroupFD = false
		cmd.SysProcAttr.CgroupFD = 0
		// Cmd cannot be started twice, even after failure. ooth supplies files
		// directly, so rebuild the launch parameters without reusing exec pipes.
		retry := &exec.Cmd{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir,
			Stdin: cmd.Stdin, Stdout: cmd.Stdout, Stderr: cmd.Stderr,
			ExtraFiles: cmd.ExtraFiles, SysProcAttr: cmd.SysProcAttr,
			NewProcessGroup: cmd.NewProcessGroup, WaitDelay: cmd.WaitDelay}
		if retryErr := retry.Start(); retryErr != nil {
			return nil, errors.Join(err, retryErr)
		}
		cmd = retry
		owner.log.Warn("cgroup placement unavailable; new workers will monitor direct children only", "error", err)
		owner.groupUnavailable = true
		job = &processJob{cmd: cmd}
	}
	owner.managed[cmd.Process.Pid] = cmd
	return job, nil
}

func (owner *processOwner) Wait(job *processJob) error {
	cmd := job.cmd
	err := cmd.Wait()
	owner.mu.Lock()
	if owner.managed[cmd.Process.Pid] == cmd {
		delete(owner.managed, cmd.Process.Pid)
	}
	owner.reap()
	owner.mu.Unlock()
	return err
}

func (owner *processOwner) Close() {
	if owner.signals != nil {
		signal.Stop(owner.signals)
		close(owner.signals)
		<-owner.done
		owner.mu.Lock()
		owner.reap()
		owner.mu.Unlock()
	}
	if owner.root != "" {
		if err := os.Remove(owner.root); err != nil {
			owner.log.Error("remove supervisor cgroup", "error", err)
		}
		if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 36, uintptr(owner.previousSubreaper), 0, 0, 0, 0); err != 0 {
			owner.log.Error("restore child subreaper", "error", err)
		}
	}
}

// SIGCHLD may coalesce. Inspect only our current children to collect adopted
// exits; membership of each worker family remains authoritative in its cgroup.
// The same lock covers Start + registration, excluding races with Cmd.Wait.
func (owner *processOwner) reap() {
	if owner.signals == nil {
		return
	}
	paths, _ := filepath.Glob("/proc/self/task/*/children")
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			owner.log.Error("read adopted children", "error", err)
		}
		for _, value := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(value)
			if err != nil || owner.managed[pid] != nil {
				continue
			}
			var status syscall.WaitStatus
			if waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); err == nil && waited > 0 {
				owner.log.Debug("adopted process exited", "pid", waited, "status", status)
			} else if err != nil && err != syscall.ECHILD && err != syscall.EINTR {
				owner.log.Error("reap adopted child", "pid", pid, "error", err)
			}
		}
	}
}

type processJob struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	file *os.File
	path string
}

func newProcessJob(cmd *exec.Cmd, root string) (*processJob, error) {
	job := &processJob{cmd: cmd}
	if root == "" {
		return job, nil
	}
	path, err := os.MkdirTemp(root, "worker-")
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	job.file, job.path = file, path
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	attr.UseCgroupFD, attr.CgroupFD = true, int(file.Fd())
	cmd.SysProcAttr = &attr
	return job, nil
}

func (job *processJob) Kill() error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.path != "" {
		if job.file == nil {
			return nil
		}
		return os.WriteFile(filepath.Join(job.path, "cgroup.kill"), []byte("1"), 0)
	}
	if job.cmd.Process != nil {
		return job.cmd.Process.Kill()
	}
	return nil
}

func (job *processJob) Close() error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.file == nil {
		return nil
	}
	// Register before killing, then read before each wait: no lost empty event.
	events, err := os.Open(filepath.Join(job.path, "cgroup.events"))
	if err != nil {
		return err
	}
	defer events.Close()
	poller, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	defer syscall.Close(poller)
	if err := syscall.EpollCtl(poller, syscall.EPOLL_CTL_ADD, int(events.Fd()), &syscall.EpollEvent{Events: syscall.EPOLLPRI | syscall.EPOLLERR}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(job.path, "cgroup.kill"), []byte("1"), 0); err != nil {
		return err
	}
	for {
		var data [256]byte
		count, err := events.ReadAt(data[:], 0)
		if count == 0 {
			return fmt.Errorf("read cgroup.events: %w", err)
		}
		if strings.Contains(string(data[:count]), "populated 0\n") {
			break
		}
		var ready [1]syscall.EpollEvent
		if _, err := syscall.EpollWait(poller, ready[:], -1); err != nil && err != syscall.EINTR {
			return err
		}
	}
	job.file.Close()
	job.file = nil
	// cgroup.kill covers nested cgroups too; remove their now-empty directories.
	var directories []string
	err = filepath.WalkDir(job.path, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			directories = append(directories, path)
		}
		return err
	})
	for index := len(directories) - 1; index >= 0 && err == nil; index-- {
		err = os.Remove(directories[index])
	}
	return err
}

func (job *processJob) Members() ([]int, error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.path == "" {
		return nil, fmt.Errorf("descendant supervision is unavailable")
	}
	if job.file == nil {
		return nil, nil
	}
	var members []int
	err := filepath.WalkDir(job.path, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || entry.Name() != "cgroup.procs" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(value)
			if err != nil {
				return err
			}
			members = append(members, pid)
		}
		return nil
	})
	return members, err
}
