//go:build ignore

// Compatibility launcher for stock Windows php-cgi's inherited FastCGI mode.
// It forwards no traffic: PHP inherits stdin and accepts on that listener.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"
)

func run() (uint32, error) {
	if len(os.Args) < 2 {
		return 1, fmt.Errorf("usage: php-worker.exe php-cgi.exe [arguments]")
	}
	stdin, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return 1, err
	}
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		return 1, err
	}
	var inherited syscall.Handle
	if err := syscall.DuplicateHandle(current, stdin, current, &inherited, 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		return 1, err
	}
	defer syscall.CloseHandle(inherited)
	var arguments []string
	for _, arg := range os.Args[1:] {
		arguments = append(arguments, syscall.EscapeArg(arg))
	}
	command, err := syscall.UTF16PtrFromString(strings.Join(arguments, " "))
	if err != nil {
		return 1, err
	}
	// The child stays in this launcher's console group and Job. A CTRL_BREAK
	// reaches PHP too; keep waiting so ooth does not end the Job prematurely.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	startup := syscall.StartupInfo{
		Cb:       uint32(unsafe.Sizeof(syscall.StartupInfo{})),
		Flags:    syscall.STARTF_USESTDHANDLES | syscall.STARTF_USESHOWWINDOW,
		StdInput: inherited, StdOutput: syscall.InvalidHandle, StdErr: syscall.InvalidHandle,
	}
	var child syscall.ProcessInformation
	if err := syscall.CreateProcess(nil, command, nil, nil, true, syscall.CREATE_UNICODE_ENVIRONMENT, nil, nil, &startup, &child); err != nil {
		return 1, err
	}
	syscall.CloseHandle(child.Thread)
	defer syscall.CloseHandle(child.Process)
	if _, err := syscall.WaitForSingleObject(child.Process, syscall.INFINITE); err != nil {
		return 1, err
	}
	var code uint32
	err = syscall.GetExitCodeProcess(child.Process, &code)
	return code, err
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(int(code))
}
