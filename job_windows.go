package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

func inheritListener(cmd *exec.Cmd, file *os.File, _ int) (uintptr, error) {
	handle := syscall.Handle(file.Fd())
	if err := syscall.SetHandleInformation(handle, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		return 0, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, handle)
	return uintptr(handle), nil
}

var jobKernel = syscall.NewLazyDLL("kernel32.dll")
var createJobObject = jobKernel.NewProc("CreateJobObjectW")
var setJobInformation = jobKernel.NewProc("SetInformationJobObject")
var queryJobInformation = jobKernel.NewProc("QueryInformationJobObject")
var terminateJobObject = jobKernel.NewProc("TerminateJobObject")
var isProcessInJob = jobKernel.NewProc("IsProcessInJob")
var jobSecurity = syscall.NewLazyDLL("advapi32.dll")
var logonUser = jobSecurity.NewProc("LogonUserW")
var getSecurityInfo = jobSecurity.NewProc("GetSecurityInfo")
var setSecurityInfo = jobSecurity.NewProc("SetSecurityInfo")
var securityDescriptorFromString = jobSecurity.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
var getDescriptorDACL = jobSecurity.NewProc("GetSecurityDescriptorDacl")
var getDescriptorControl = jobSecurity.NewProc("GetSecurityDescriptorControl")

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

// Operate on the AF_UNIX reparse point itself, without following its tag.
func (identity Identity) socket(path string, _ os.FileMode) (func(bool) error, error) {
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	account := current
	if identity.User != "" {
		account, err = user.Lookup(identity.User)
		if err != nil {
			return nil, err
		}
	}
	owner, err := syscall.StringToSid(account.Uid)
	if err != nil {
		return nil, err
	}
	// Keep supervisor access for reload/cleanup; do not inherit broad directory ACLs.
	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;" + account.Uid + ")"
	if current.Uid != account.Uid {
		sddl += "(A;;FA;;;" + current.Uid + ")"
	}
	text, _ := syscall.UTF16PtrFromString(sddl)
	var descriptor unsafe.Pointer
	if ok, _, err := securityDescriptorFromString.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0); ok == 0 {
		return nil, err
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(descriptor)))
	var dacl unsafe.Pointer
	var present, defaulted uint32
	if ok, _, err := getDescriptorDACL.Call(uintptr(descriptor), uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); ok == 0 {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%s is not a socket", path)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(name, 0xE0000, 7, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0) // READ_CONTROL | WRITE_DAC | WRITE_OWNER
	if err != nil {
		return nil, err
	}
	var previous, previousOwner, previousDACL unsafe.Pointer
	// SE_FILE_OBJECT, OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION.
	if code, _, _ := getSecurityInfo.Call(uintptr(handle), 1, 5, uintptr(unsafe.Pointer(&previousOwner)), 0, uintptr(unsafe.Pointer(&previousDACL)), 0, uintptr(unsafe.Pointer(&previous))); code != 0 {
		syscall.CloseHandle(handle)
		return nil, syscall.Errno(code)
	}
	var control uint16
	var revision uint32
	if ok, _, err := getDescriptorControl.Call(uintptr(previous), uintptr(unsafe.Pointer(&control)), uintptr(unsafe.Pointer(&revision))); ok == 0 {
		syscall.LocalFree(syscall.Handle(uintptr(previous)))
		syscall.CloseHandle(handle)
		return nil, err
	}
	finish := func(commit bool) error {
		defer syscall.LocalFree(syscall.Handle(uintptr(previous)))
		var restoreErr error
		if !commit {
			flags := uintptr(5 | 0x20000000) // UNPROTECTED_DACL_SECURITY_INFORMATION
			if control&0x1000 != 0 {         // SE_DACL_PROTECTED
				flags = 5 | 0x80000000
			}
			if code, _, _ := setSecurityInfo.Call(uintptr(handle), 1, flags, uintptr(previousOwner), 0, uintptr(previousDACL), 0); code != 0 {
				restoreErr = syscall.Errno(code)
			}
		}
		return errors.Join(restoreErr, syscall.CloseHandle(handle))
	}
	if code, _, _ := setSecurityInfo.Call(uintptr(handle), 1, 5|0x80000000, uintptr(unsafe.Pointer(owner)), 0, uintptr(dacl), 0); code != 0 {
		return nil, errors.Join(syscall.Errno(code), finish(false))
	}
	return finish, nil
}

type processOwner struct{}

func (*processOwner) resourceGroups() (map[string]resourceGroup, error) { return nil, nil }

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
