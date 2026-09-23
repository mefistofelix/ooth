package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var jobKernel = syscall.NewLazyDLL("kernel32.dll")
var createJobObject = jobKernel.NewProc("CreateJobObjectW")
var setJobInformation = jobKernel.NewProc("SetInformationJobObject")
var queryJobInformation = jobKernel.NewProc("QueryInformationJobObject")
var terminateJobObject = jobKernel.NewProc("TerminateJobObject")
var isProcessInJob = jobKernel.NewProc("IsProcessInJob")
var logonUser = syscall.NewLazyDLL("advapi32.dll").NewProc("LogonUserW")

func (identity Identity) apply(cmd *exec.Cmd) (func(), error) {
	if identity.Group != "" {
		return nil, fmt.Errorf("group is only supported on Linux")
	}
	if identity.User == "" {
		if identity.Password != nil {
			return nil, fmt.Errorf("password requires user")
		}
		return func() {}, nil
	}
	if identity.Password == nil {
		account, err := user.Lookup(identity.User)
		if err != nil {
			return nil, err
		}
		current, err := user.Current()
		if err != nil {
			return nil, err
		}
		if account.Uid != current.Uid {
			return nil, fmt.Errorf("password is required to obtain a token for Windows account %q", identity.User)
		}
		return func() {}, nil
	}
	username, domain := identity.User, "."
	if prefix, suffix, ok := strings.Cut(username, `\`); ok {
		domain, username = prefix, suffix
	} else if strings.Contains(username, "@") {
		domain = "" // UPN: LogonUser requires a null domain pointer.
	}
	name, err := syscall.UTF16PtrFromString(username)
	if err != nil {
		return nil, err
	}
	var domainName *uint16
	if domain != "" {
		domainName, err = syscall.UTF16PtrFromString(domain)
		if err != nil {
			return nil, err
		}
	}
	password, err := syscall.UTF16FromString(*identity.Password)
	if err != nil {
		return nil, err
	}
	defer clear(password)
	var token syscall.Token
	// LOGON32_LOGON_BATCH obtains a primary token for unattended workers.
	if ok, _, err := logonUser.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(domainName)), uintptr(unsafe.Pointer(&password[0])), 4, 0, uintptr(unsafe.Pointer(&token))); ok == 0 {
		return nil, fmt.Errorf("LogonUser for %q: %w", identity.User, err)
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	attr.Token = token
	cmd.SysProcAttr = &attr
	return func() { token.Close() }, nil
}

type processOwner struct{}

func newProcessOwner(_ string, _ *slog.Logger) (*processOwner, error) {
	return &processOwner{}, nil
}

func (owner *processOwner) Start(cmd *exec.Cmd) (*processJob, error) {
	job, err := newProcessJob(cmd, "")
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		job.Close()
		return nil, err
	}
	return job, nil
}

func (owner *processOwner) Wait(job *processJob) error { return job.cmd.Wait() }
func (owner *processOwner) Close()                     {}

// Native JOBOBJECT_EXTENDED_LIMIT_INFORMATION layout on amd64 and arm64.
type jobLimits struct {
	ProcessTime, JobTime             int64
	Flags                            uint32
	MinWorkingSet, MaxWorkingSet     uintptr
	ActiveProcesses                  uint32
	Affinity                         uintptr
	Priority, Scheduling             uint32
	IOCounters                       [6]uint64
	ProcessMemory, JobMemory         uintptr
	PeakProcessMemory, PeakJobMemory uintptr
}

// The handle is private to ooth. Descendants inherit membership, not this handle.
type processJob struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	handle syscall.Handle
}

func newProcessJob(cmd *exec.Cmd, _ string) (*processJob, error) {
	handle, _, err := createJobObject.Call(0, 0)
	if handle == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	job := &processJob{cmd: cmd, handle: syscall.Handle(handle)}
	// No BREAKAWAY flags: even a descendant whose parent dies stays owned.
	limits := jobLimits{Flags: 0x2000} // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if ok, _, err := setJobInformation.Call(handle, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits)); ok == 0 {
		job.Close()
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	attr.JobObjects = append(append([]syscall.Handle{}, attr.JobObjects...), job.handle)
	cmd.SysProcAttr = &attr
	return job, nil
}

func (job *processJob) Kill() error {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.kill()
}

func (job *processJob) kill() error {
	if job.handle == 0 {
		return nil
	}
	if ok, _, err := terminateJobObject.Call(uintptr(job.handle), 1); ok == 0 {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

func (job *processJob) Close() error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.handle == 0 {
		return nil
	}
	if err := job.kill(); err != nil {
		return err
	}
	// Drain any processes still terminating before a replacement can start.
	// Membership comes from the Job; waits use process handles, not PID polling.
	members, err := job.members()
	if err != nil {
		return err
	}
	for _, pid := range members {
		handle, err := syscall.OpenProcess(0x100000|0x1000, false, uint32(pid)) // SYNCHRONIZE | QUERY_LIMITED_INFORMATION
		if err == syscall.Errno(87) {                                           // ERROR_INVALID_PARAMETER
			continue // Already exited; there is no process object to wait for.
		}
		if err != nil {
			return err
		}
		var belongs uint32
		ok, _, queryErr := isProcessInJob.Call(uintptr(handle), uintptr(job.handle), uintptr(unsafe.Pointer(&belongs)))
		if ok == 0 {
			syscall.CloseHandle(handle)
			return queryErr
		}
		if belongs != 0 { // A recycled PID must never make us wait for an unrelated process.
			_, err = syscall.WaitForSingleObject(handle, syscall.INFINITE)
		}
		syscall.CloseHandle(handle)
		if err != nil {
			return err
		}
	}
	err = syscall.CloseHandle(job.handle)
	if err == nil {
		job.handle = 0
	}
	return err
}

// Members queries kernel membership on demand; it never reconstructs ancestry.
func (job *processJob) Members() ([]int, error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.members()
}

func (job *processJob) members() ([]int, error) {
	if job.handle == 0 {
		return nil, nil
	}
	for capacity := 16; ; capacity *= 2 {
		// Two DWORD counts followed by capacity ULONG_PTR process IDs.
		buffer := make([]uintptr, capacity+1)
		ok, _, err := queryJobInformation.Call(uintptr(job.handle), 3, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer))*unsafe.Sizeof(buffer[0]), 0)
		if ok == 0 {
			if err == syscall.ERROR_MORE_DATA {
				continue
			}
			return nil, fmt.Errorf("QueryInformationJobObject: %w", err)
		}
		counts := (*[2]uint32)(unsafe.Pointer(&buffer[0]))
		members := make([]int, counts[1])
		for index := range members {
			members[index] = int(buffer[index+1])
		}
		return members, nil
	}
}
