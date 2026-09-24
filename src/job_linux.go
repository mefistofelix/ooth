package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

func inheritListener(cmd *exec.Cmd, file *os.File, fd int) (uintptr, error) {
	for len(cmd.ExtraFiles) < fd-3 {
		cmd.ExtraFiles = append(cmd.ExtraFiles, nil)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return uintptr(2 + len(cmd.ExtraFiles)), nil
}

// Read every visible ancestor: a sibling may exhaust a parent's budget even
// while this supervisor's own subtree is mostly idle.
func (owner *processOwner) resourceGroups() (map[string]resourceGroup, error) {
	membership, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	paths := resourceGroupPaths(string(membership), string(mounts), owner.root)
	groups := make(map[string]resourceGroup)
	for _, path := range paths {
		group, err := readResourceGroup(path)
		if err != nil {
			return nil, fmt.Errorf("resource cgroup %s: %w", path, err)
		}
		groups[path] = group
	}
	return groups, nil
}

func resourceGroupPaths(membership, mounts, workerRoot string) []string {
	var current string
	for _, line := range strings.Split(membership, "\n") {
		if name, ok := strings.CutPrefix(line, "0::"); ok {
			current = name
		}
	}
	var paths []string
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(mounts, "\n") {
		before, _, ok := strings.Cut(line, " - cgroup2 ")
		if !ok {
			continue
		}
		fields := strings.Fields(before)
		mountRoot, mountPoint := unescape.Replace(fields[3]), unescape.Replace(fields[4])
		var roots []string
		if relative, err := filepath.Rel(mountRoot, current); current != "" && err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
			roots = append(roots, filepath.Join(mountPoint, relative))
		}
		if relative, err := filepath.Rel(mountPoint, workerRoot); workerRoot != "" && err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
			roots = append(roots, workerRoot)
		}
		for _, root := range roots {
			for path := root; ; path = filepath.Dir(path) {
				if !slices.Contains(paths, path) {
					paths = append(paths, path)
				}
				if path == mountPoint {
					break
				}
			}
		}
	}
	return paths
}

func readResourceGroup(path string) (resourceGroup, error) {
	group := resourceGroup{availableMemory: 100}
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(path, name))
		if os.IsNotExist(err) {
			return "", nil
		} // Controller not enabled here.
		return strings.TrimSpace(string(data)), err
	}
	quota, err := read("cpu.max")
	if err != nil {
		return group, err
	}
	if quota != "" {
		fields := strings.Fields(quota)
		if len(fields) != 2 {
			return group, fmt.Errorf("invalid cpu.max")
		}
		if fields[0] != "max" {
			budget, budgetErr := strconv.ParseFloat(fields[0], 64)
			period, periodErr := strconv.ParseFloat(fields[1], 64)
			if budgetErr != nil || periodErr != nil || budget <= 0 || period <= 0 {
				return group, fmt.Errorf("invalid cpu.max quota")
			}
			group.cores = budget / period
		}
	}
	cpuset, err := read("cpuset.cpus.effective")
	if err != nil {
		return group, err
	}
	if cpuset != "" {
		count := 0
		for _, part := range strings.Split(cpuset, ",") {
			first, last, span := strings.Cut(part, "-")
			low, err := strconv.Atoi(first)
			if err != nil {
				return group, err
			}
			high := low
			if span {
				high, err = strconv.Atoi(last)
			}
			if err != nil || high < low {
				return group, fmt.Errorf("invalid cpuset range")
			}
			count += high - low + 1
		}
		if group.cores == 0 || float64(count) < group.cores {
			group.cores = float64(count)
		}
	}
	if group.cores > 0 {
		data, err := read("cpu.stat")
		if err != nil {
			return group, err
		}
		found := false
		for _, line := range strings.Split(data, "\n") {
			if value, ok := strings.CutPrefix(line, "usage_usec "); ok {
				usage, err := strconv.ParseUint(value, 10, 64)
				if err != nil {
					return group, err
				}
				group.cpuSeconds = float64(usage) / 1e6
				found = true
			}
		}
		if !found {
			return group, fmt.Errorf("cpu.stat missing usage_usec")
		}
	}
	limit, err := read("memory.max")
	if err != nil {
		return group, err
	}
	if limit != "" && limit != "max" {
		maximum, err := strconv.ParseUint(limit, 10, 64)
		if err != nil {
			return group, err
		}
		value, err := read("memory.current")
		if err != nil {
			return group, err
		}
		used, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return group, err
		}
		group.availableMemory = 0
		if maximum > used {
			group.availableMemory = 100 * float64(maximum-used) / float64(maximum)
		}
	}
	return group, nil
}

func (identity Identity) apply(cmd *exec.Cmd) (func(), error) {
	credential, err := identity.credentials()
	if err != nil {
		return nil, err
	}
	if credential != nil {
		attr := syscall.SysProcAttr{}
		if cmd.SysProcAttr != nil {
			attr = *cmd.SysProcAttr
		}
		attr.Credential = credential
		cmd.SysProcAttr = &attr
	}
	return func() {}, nil
}

func (identity Identity) credentials() (*syscall.Credential, error) {
	if identity.Password != nil {
		return nil, fmt.Errorf("password is only supported on Windows")
	}
	if identity.User == "" && identity.Group == "" {
		return nil, nil
	}
	var account *user.User
	var err error
	if identity.User == "" {
		account, err = user.LookupId(strconv.Itoa(os.Geteuid()))
	} else if _, numeric := strconv.ParseUint(identity.User, 10, 32); numeric == nil {
		account, err = user.LookupId(identity.User)
	} else {
		account, err = user.Lookup(identity.User)
	}
	if err != nil {
		return nil, err
	}
	groupID := account.Gid
	if identity.Group != "" {
		var group *user.Group
		if _, numeric := strconv.ParseUint(identity.Group, 10, 32); numeric == nil {
			group, err = user.LookupGroupId(identity.Group)
		} else {
			group, err = user.LookupGroup(identity.Group)
		}
		if err != nil {
			return nil, err
		}
		groupID = group.Gid
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(groupID, 10, 32)
	if err != nil {
		return nil, err
	}
	groups, err := account.GroupIds()
	if err != nil {
		return nil, err
	}
	credential := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	for _, group := range groups {
		value, err := strconv.ParseUint(group, 10, 32)
		if err != nil {
			return nil, err
		}
		credential.Groups = append(credential.Groups, uint32(value))
	}
	// An unprivileged caller cannot call setgroups even for its own identity.
	credential.NoSetGroups = os.Geteuid() != 0 && credential.Uid == uint32(os.Geteuid())
	return credential, nil
}

// The returned function commits or restores permissions during config reload.
func (identity Identity) socket(path string, mode os.FileMode) (func(bool) error, error) {
	credential, err := identity.credentials()
	if err != nil {
		return nil, err
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if credential != nil {
		uid, gid = int(credential.Uid), int(credential.Gid)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%s is not a socket", path)
	}
	previous := info.Sys().(*syscall.Stat_t)
	finish := func(commit bool) error {
		if commit {
			return nil
		}
		return errors.Join(os.Chown(path, int(previous.Uid), int(previous.Gid)), os.Chmod(path, info.Mode().Perm()))
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		return nil, errors.Join(err, finish(false))
	}
	return finish, nil
}

type processOwner struct {
	mu               sync.Mutex
	root             string
	managed          map[int]*exec.Cmd
	log              *slog.Logger
	signals          chan os.Signal
	done             chan struct{}
	resetSubreaper   bool
	groupUnavailable bool
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
		logger.Warn("descendant supervision disabled; group termination unavailable", "error", err)
	}
	// PID 1 already adopts orphans. Other supervisors opt into that role even
	// without cgroups: adoption and service membership are independent.
	reaping := os.Getpid() == 1
	if !reaping {
		var previous int32
		_, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 37, uintptr(unsafe.Pointer(&previous)), 0, 0, 0, 0) // PR_GET_CHILD_SUBREAPER
		if err == 0 && previous == 0 {
			_, _, err = syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0) // PR_SET_CHILD_SUBREAPER
			owner.resetSubreaper = err == 0
		}
		reaping = err == 0
		if err != 0 {
			logger.Warn("orphan adoption unavailable; direct worker exits are still monitored", "error", err)
		}
	}
	if reaping {
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
	}
	if owner.resetSubreaper {
		if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 36, 0, 0, 0, 0, 0); err != 0 {
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
