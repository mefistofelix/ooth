package main

import (
	"errors"
	"io"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func testEpoll(t *testing.T) (int, *Listener, int) {
	t.Helper()
	port, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.EpollClose(port); err != nil {
			t.Error(err)
		}
	})
	listener, err := OpenListener("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fd := int(listener.File.Fd())
	t.Cleanup(func() {
		syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil)
		listener.Close()
	})
	return port, listener, fd
}

func epollWait(t *testing.T, port int, timeout int) []syscall.EpollEvent {
	t.Helper()
	events := make([]syscall.EpollEvent, 64)
	for {
		count, err := syscall.EpollWait(port, events, timeout)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		return events[:count]
	}
}

func TestEpollListenerReadiness(t *testing.T) {
	for _, oneShot := range []bool{false, true} {
		name := "level"
		if oneShot {
			name = "oneshot"
		}
		t.Run(name, func(t *testing.T) {
			port, listener, fd := testEpoll(t)
			event := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: 123, Pad: 456}
			if oneShot {
				event.Events |= syscall.EPOLLONESHOT
			}
			if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
				t.Fatal(err)
			}
			if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &event); !errors.Is(err, syscall.EEXIST) {
				t.Fatalf("duplicate: %v", err)
			}
			if got := epollWait(t, port, 15); len(got) != 0 {
				t.Fatalf("empty listener readable: %v", got)
			}
			client, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			client.Write([]byte("payload"))
			got := epollWait(t, port, 1000)
			if len(got) != 1 || got[0].Fd != 123 || got[0].Pad != 456 || got[0].Events != syscall.EPOLLIN {
				t.Fatalf("readiness/data: %v", got)
			}
			got = epollWait(t, port, 15)
			if oneShot && len(got) != 0 {
				t.Fatalf("oneshot repeated: %v", got)
			}
			if !oneShot && len(got) != 1 {
				t.Fatalf("level readiness lost: %v", got)
			}
			event.Fd = 789
			if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_MOD, fd, &event); err != nil {
				t.Fatal(err)
			}
			got = epollWait(t, port, 1000)
			if len(got) != 1 || got[0].Fd != 789 {
				t.Fatalf("rearm data: %v", got)
			}
			// Accept only in the test: prove EpollWait left connection and bytes intact.
			accepted, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer accepted.Close()
			accepted.SetReadDeadline(time.Now().Add(time.Second))
			buffer := make([]byte, 7)
			count, err := io.ReadFull(accepted, buffer)
			if err != nil || string(buffer[:count]) != "payload" {
				t.Fatalf("payload: %q %v", buffer[:count], err)
			}
		})
	}
}

func TestEpollPendingChanges(t *testing.T) {
	port, listener, fd := testEpoll(t)
	for index := int32(1); index <= 30; index++ {
		event := syscall.EpollEvent{Events: syscall.EPOLLIN | syscall.EPOLLONESHOT, Fd: index}
		if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
			t.Fatal(err)
		}
		event.Fd += 100
		if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_MOD, fd, &event); err != nil {
			t.Fatal(err)
		}
		if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil); err != nil {
			t.Fatal(err)
		}
	}
	event := syscall.EpollEvent{Events: syscall.EPOLLIN | syscall.EPOLLONESHOT, Fd: 999}
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	runtime.GC() // Native pending requests must retain and pin their buffers.
	got := epollWait(t, port, 1000)
	if len(got) != 1 || got[0].Fd != 999 {
		t.Fatalf("stale registration delivered: %v", got)
	}
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil); err != nil {
		t.Fatal(err)
	}
	if got := epollWait(t, port, 15); len(got) != 0 {
		t.Fatalf("deleted event: %v", got)
	}
}

func TestEpollInvalidOperations(t *testing.T) {
	port, _, fd := testEpoll(t)
	if _, err := syscall.EpollCreate1(-1); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("invalid flags: %v", err)
	}
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("missing registration: %v", err)
	}
	if err := syscall.EpollCtl(port, 99, fd, &syscall.EpollEvent{}); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("invalid operation: %v", err)
	}
	if _, err := syscall.EpollWait(port, nil, 0); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("empty output: %v", err)
	}
	if runtime.GOOS == "windows" {
		if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: 1 << 31}); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("unsupported edge triggering: %v", err)
		}
	}
}

func TestEpollConcurrentControl(t *testing.T) {
	port, listener, fd := testEpoll(t)
	type result struct {
		events []syscall.EpollEvent
		err    error
	}
	done := make(chan result, 1)
	go func() {
		events := make([]syscall.EpollEvent, 1)
		for {
			count, err := syscall.EpollWait(port, events, 1000)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				count = 0
			}
			done <- result{events[:count], err}
			return
		}
	}()
	event := syscall.EpollEvent{Events: syscall.EPOLLIN | syscall.EPOLLONESHOT, Fd: 55}
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case got := <-done:
		if got.err != nil || len(got.events) != 1 || got.events[0].Fd != 55 {
			t.Fatalf("concurrent add: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not wake")
	}
}

func TestEpollWindowsCloseWait(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Linux close does not interrupt epoll_wait")
	}
	port, err := syscall.EpollCreate1(0)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := OpenListener("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, int(listener.File.Fd()), &syscall.EpollEvent{Events: syscall.EPOLLIN}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := syscall.EpollWait(port, make([]syscall.EpollEvent, 1), -1); done <- err }()
	time.Sleep(10 * time.Millisecond)
	if err := syscall.EpollClose(port); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, syscall.EBADF) {
			t.Fatalf("closed wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake wait")
	}
}

func TestEpollConnectionEvents(t *testing.T) {
	port, listener, _ := testEpoll(t)
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	raw, err := peer.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd int
	if err := raw.Control(func(handle uintptr) { fd = int(handle) }); err != nil {
		t.Fatal(err)
	}
	defer syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil)
	event := syscall.EpollEvent{Events: syscall.EPOLLOUT | syscall.EPOLLONESHOT, Fd: 77}
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
		t.Fatal(err)
	}
	got := epollWait(t, port, 1000)
	if len(got) != 1 || got[0].Events&syscall.EPOLLOUT == 0 {
		t.Fatalf("writable: %v", got)
	}
	event.Events = syscall.EPOLLIN | syscall.EPOLLRDHUP | syscall.EPOLLONESHOT
	if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_MOD, fd, &event); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got = epollWait(t, port, 1000)
	if len(got) != 1 || got[0].Events&syscall.EPOLLRDHUP == 0 {
		t.Fatalf("peer half-close: %v", got)
	}
}

func TestEpollManyListeners(t *testing.T) {
	port, _, _ := testEpoll(t)
	before := runtime.NumGoroutine()
	for index := 0; index < 128; index++ {
		listener, err := OpenListener("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		fd := int(listener.File.Fd())
		defer func() { syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil); listener.Close() }()
		if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN}); err != nil {
			t.Fatal(err)
		}
	}
	if growth := runtime.NumGoroutine() - before; growth > 2 {
		t.Fatalf("128 idle listeners added %d goroutines", growth)
	}
	if got := epollWait(t, port, 10); len(got) != 0 {
		t.Fatalf("idle events: %v", got)
	}
	runtime.GC()
}

func BenchmarkEpollIdleListeners(b *testing.B) {
	port, err := syscall.EpollCreate1(0)
	if err != nil {
		b.Fatal(err)
	}
	defer syscall.EpollClose(port)
	for index := 0; index < 128; index++ {
		listener, err := OpenListener("tcp4", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		fd := int(listener.File.Fd())
		defer func() { syscall.EpollCtl(port, syscall.EPOLL_CTL_DEL, fd, nil); listener.Close() }()
		if err := syscall.EpollCtl(port, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(index)}); err != nil {
			b.Fatal(err)
		}
	}
	events := make([]syscall.EpollEvent, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		count, err := syscall.EpollWait(port, events, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || count != 0 {
			b.Fatalf("idle wait: %d %v", count, err)
		}
	}
	b.StopTimer()
}
