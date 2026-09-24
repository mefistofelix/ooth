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
	replace(*root, "src/internal/poll/fd_windows.go", "\terr := fd.pd.init(fd)\n\tif err != nil {\n\t\treturn err\n\t}\n", `	err := fd.pd.init(fd)
	if err != nil {
		// ooth: like libuv's imported-socket fallback, retain normal IOCP
		// unless a shared listener cannot join this process's completion port.
		if err != windows.ERROR_INVALID_PARAMETER || fd.kind != kindNet {
			return err
		}
		listening, socketErr := syscall.GetsockoptInt(fd.Sysfd, syscall.SOL_SOCKET, 2)
		if socketErr != nil || listening == 0 {
			return err
		}
		ctx, errno := runtime_pollOpen(uintptr(syscall.InvalidHandle))
		if errno != 0 {
			return syscall.Errno(errno)
		}
		fd.pd.runtimeCtx = ctx
		return nil
	}
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
	replace(*root, "src/syscall/exec_windows.go", "type SysProcAttr struct {\n", `type SysProcAttr struct {
	// JobObjects assigns the child to these jobs during creation, before it runs.
	// Requires Windows 10 / Server 2016. Added by the ooth toolchain patch.
	JobObjects []Handle
`)
	replace(*root, "src/syscall/exec_windows.go",
		"procAttrList, err := newProcThreadAttributeList(2)",
		"procAttrList, err := newProcThreadAttributeList(3)")
	replace(*root, "src/syscall/exec_windows.go", "\tsi.StdInput = fd[0]\n", `	if len(sys.JobObjects) > 0 {
		const procThreadAttributeJobList = 0x0002000D
		err = procAttrList.update(procThreadAttributeJobList, unsafe.Pointer(&sys.JobObjects[0]), uintptr(len(sys.JobObjects))*unsafe.Sizeof(sys.JobObjects[0]))
		if err != nil {
			return 0, 0, err
		}
	}
	si.StdInput = fd[0]
`)
	for template, target := range map[string]string{
		"epoll_linux.txt":   "src/syscall/ooth_epoll_linux.go",
		"epoll_windows.txt": "src/syscall/ooth_epoll_windows.go",
		"poll_windows.txt":  "src/internal/poll/ooth_windows.go",
		"exec_windows.txt":  "src/os/exec/ooth_windows.go",
		"exec_other.txt":    "src/os/exec/ooth_other.go",
		"os_windows.txt":    "src/os/ooth_windows.go",
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
