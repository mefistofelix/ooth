// patchgo modifies only the dedicated Go toolchain passed with -goroot.
package main

import (
	"embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed patches/*.txt
var patches embed.FS

func main() {
	root := flag.String("goroot", "", "dedicated Go 1.27.1 installation")
	flag.Parse()
	if *root == "" {
		fail(fmt.Errorf("-goroot is required"))
	}
	version, err := os.ReadFile(filepath.Join(*root, "VERSION"))
	if err != nil {
		fail(err)
	}
	if strings.Split(string(version), "\n")[0] != "go1.27.1" {
		fail(fmt.Errorf("patch requires Go 1.27.1"))
	}
	replace(*root, "src/internal/poll/fd_unix.go",
		"\t\treturn 0, nil\n\t}\n\tif err := fd.pd.prepareRead(fd.isFile)",
		`		// ooth: retain the pending edge; do not reset it with prepareRead.
		return 0, fd.pd.waitRead(fd.isFile)
	}
	if err := fd.pd.prepareRead(fd.isFile)`)
	replace(*root, "src/internal/poll/fd_windows.go",
		"\tif len(buf) > maxRW {\n\t\tbuf = buf[:maxRW]\n\t}\n\n\tvar n int",
		`	// ooth: listener readiness without accepting a connection.
	if len(buf) == 0 && fd.sharedListener {
		return 0, fd.waitSocketReadable()
	}
	if len(buf) > maxRW {
		buf = buf[:maxRW]
	}

	var n int`)
	replace(*root, "src/internal/poll/fd_windows.go", "\twaitOnSuccess bool\n", "\twaitOnSuccess bool\n\tsharedListener bool // ooth: no process owns the shared socket's IOCP\n")
	replace(*root, "src/internal/poll/fd_windows.go", "\terr := fd.pd.init(fd)\n", `	// ooth: shared listening sockets must not belong to one process's IOCP.
	if listening, _ := syscall.GetsockoptInt(fd.Sysfd, syscall.SOL_SOCKET, 2); fd.kind == kindNet && listening != 0 {
		serverInit.Do(runtime_pollServerInit)
		fd.sharedListener = true
		ctx, errno := runtime_pollOpen(uintptr(syscall.InvalidHandle))
		if errno != 0 { return syscall.Errno(errno) }
		fd.pd.runtimeCtx = ctx
		return nil
	}
	err := fd.pd.init(fd)
`)
	replace(*root, "src/runtime/netpoll_windows.go", "func netpollopen(fd uintptr, pd *pollDesc) int32 {\n", `func netpollopen(fd uintptr, pd *pollDesc) int32 {
	// ooth: deadline/close tracking for a listener shared between processes.
	if fd == ^uintptr(0) { return 0 }
`)
	replace(*root, "src/internal/poll/fd_windows.go", "\t\t_, err := syscall.WaitForSingleObject(o.o.HEvent, syscall.INFINITE)\n\t\treturn err", "\t\treturn fd.waitSharedIO(o)")
	replace(*root, "src/os/exec/exec.go", "\tSysProcAttr *syscall.SysProcAttr\n",
		`	SysProcAttr *syscall.SysProcAttr

	// NewProcessGroup creates a separately signalable console group on Windows.
	// It has no effect on other platforms. Added by the ooth toolchain patch.
	NewProcessGroup bool
`)
	replace(*root, "src/os/exec/exec.go", "\tc.Process, err = os.StartProcess(lp, c.argv(), &os.ProcAttr{",
		`	if err := c.prepareProcessGroup(); err != nil {
		return err
	}
	c.Process, err = os.StartProcess(lp, c.argv(), &os.ProcAttr{`)
	replace(*root, "src/os/exec_windows.go", "\tif sig == Kill {",
		`	// ooth: CTRL_BREAK supports graceful shutdown of a console process group.
	if sig == Interrupt {
		return signalConsoleGroup(p.Pid)
	}
	if sig == Kill {`)
	replace(*root, "src/syscall/exec_windows.go", "\t\tif attr.Files[i] > 0 {\n\t\t\terr := DuplicateHandle",
		`		if attr.Files[i] > 0 {
			// ooth: inherit Winsock handles directly; DuplicateHandle loses socket context.
			if _, socketErr := GetsockoptInt(Handle(attr.Files[i]), SOL_SOCKET, 0x1008); socketErr == nil {
				if parentProcess != p { return 0, 0, EWINDOWS }
				fd[i] = Handle(attr.Files[i])
				if err := SetHandleInformation(fd[i], HANDLE_FLAG_INHERIT, HANDLE_FLAG_INHERIT); err != nil { return 0, 0, err }
				defer SetHandleInformation(fd[i], HANDLE_FLAG_INHERIT, 0)
				continue
			}
			err := DuplicateHandle`)
	for template, target := range map[string]string{
		"poll_windows.txt": "src/internal/poll/ooth_windows.go",
		"exec_windows.txt": "src/os/exec/ooth_windows.go",
		"exec_other.txt":   "src/os/exec/ooth_other.go",
		"os_windows.txt":   "src/os/ooth_windows.go",
	} {
		data, err := patches.ReadFile("patches/" + template)
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(filepath.Join(*root, target), data, 0644); err != nil {
			fail(err)
		}
	}
}

func replace(root, path, before, after string) {
	path = filepath.Join(root, path)
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.Contains(source, after) {
		return
	}
	if strings.Count(source, before) != 1 {
		fail(fmt.Errorf("%s: expected patch location exactly once", path))
	}
	if err := os.WriteFile(path, []byte(strings.Replace(source, before, after, 1)), 0644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
