package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

// The child keeps the native handle value, not a CRT descriptor numbered 3.
func extraTestHandle(t *testing.T, cmd *exec.Cmd, file *os.File) (string, func()) {
	t.Helper()
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	var handle syscall.Handle
	if err := syscall.DuplicateHandle(current, syscall.Handle(file.Fd()), current, &handle, 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{handle}}
	return strconv.FormatUint(uint64(handle), 10), func() { syscall.CloseHandle(handle) }
}
