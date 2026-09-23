//go:build ignore

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if os.Getenv("HANDOFF_CHILD") == "1" {
		serve()
		return
	}
	if len(os.Args) < 2 {
		panic("usage: socket_handoff_windows.exe WORKER [ARG ...]")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	// Match ooth: keep an observation duplicate plus a separate copy per child.
	observation, err := listener.File()
	if err != nil {
		panic(err)
	}
	defer observation.Close()
	fmt.Fprintf(os.Stderr, "parent=%d observation=%d\n", os.Getpid(), observation.Fd())
	for index := 0; index < 3; index++ {
		file, err := listener.File()
		if err != nil {
			panic(err)
		}
		defer file.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[1], os.Args[2:]...)
		cmd.Stdin = file
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "HANDOFF_CHILD=1")
		if os.Getenv("HANDOFF_RAW") == "1" {
			// Diagnostic only: pass a known native handle to Node's private API.
			handle := syscall.Handle(file.Fd())
			if err := syscall.SetHandleInformation(handle, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
				panic(err)
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{handle}}
			cmd.Env = append(cmd.Env, "HANDOFF_HANDLE="+strconv.FormatUint(uint64(handle), 10))
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			panic(err)
		}
		if err := cmd.Start(); err != nil {
			panic(err)
		}
		file.Close()
		defer func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		}()
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != "ready" {
			panic(fmt.Errorf("worker %d did not become ready: %v", index, cmd.Wait()))
		}
		fmt.Printf("worker=%d pid=%d ready\n", index, cmd.Process.Pid)
		// Older workers stay alive and may accept first; require this one's reply.
		for {
			if ctx.Err() != nil {
				panic(ctx.Err())
			}
			connection, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
			if err != nil {
				panic(err)
			}
			connection.SetReadDeadline(time.Now().Add(time.Second))
			reply, err := bufio.NewReader(connection).ReadString('\n')
			connection.Close()
			if err != nil {
				panic(err)
			}
			if strings.TrimSpace(reply) == strconv.Itoa(cmd.Process.Pid) {
				fmt.Printf("worker=%d reply=%s", index, reply)
				break
			}
		}
		if os.Getenv("HANDOFF_SEQUENTIAL") == "1" {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}
}

func serve() {
	handle := syscall.Handle(os.Stdin.Fd())
	listening, err := syscall.GetsockoptInt(handle, syscall.SOL_SOCKET, 2)
	fmt.Fprintf(os.Stderr, "stdin=%d listening=%d socketErr=%v\n", handle, listening, err)
	listener, err := net.FileListener(os.Stdin)
	if err != nil {
		panic(fmt.Errorf("FileListener: %w", err))
	}
	defer listener.Close()
	os.Stdin.Close()
	fmt.Println("ready")
	for {
		connection, err := listener.Accept()
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(connection, os.Getpid())
		connection.Close()
	}
}
