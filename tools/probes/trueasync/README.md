# TrueAsync 0.10.0 compatibility audit

The target is TrueAsync's built-in HTTP/1.1 + HTTP/2 server adopting ooth's
listener, one PHP server thread with coroutines, request telemetry and graceful
shutdown. The current extra-handle convention avoids the historical stdin
failure below. **Windows now passes the full ooth lifecycle through a private
FFI hook; Linux's released binary lacks FFI and remains limited to async accept.**
No runtime is recompiled, as explicitly requested. Independent control listeners
are diagnostics, not replacements for socket activation.

`worker.php` is the new protocol worker. The [common matrix](../runtime/README.md)
checks HTTP/1.1 and h2c directly and through Caddy/Nginx, cold activation, growth,
both serving PIDs, balanced events, idle zero, reactivation and graceful draining.
HTTP/1.1 cases also exercise the native WebSocket server: text/binary echo,
ping/pong, survival beyond idle TTL and both close directions. The explicit
`recv()` loop and native `stop()` retain active HTTP responses during shutdown.
This remains a pinned private-ABI adapter, not a public adoption API.

## Reproduce

The preparation scripts download the official PHP 8.6 TrueAsync 0.10.0 release
and verify its published GitHub asset SHA256. Files remain under ignored
`build/runtime-trueasync`; nothing is installed globally. The Windows server and
sockets extensions are included in that distribution and enabled by the test.

Windows, after building ooth:

```powershell
./tools/probes/trueasync/prepare-windows.ps1
$env:CGO_ENABLED = '0'
$env:OOTH_TEST_TRUEASYNC = "$PWD/build/runtime-trueasync/windows/php.exe"
./build/windows-amd64/go/bin/go.exe test -v -run '^TestTrueAsyncCompatibility$' .
./build/windows-amd64/go/bin/go.exe test -v -run '^TestTrueAsyncExtraHandle$' .
```

Linux:

```sh
bash tools/probes/trueasync/prepare-linux.sh
CGO_ENABLED=0 OOTH_TEST_TRUEASYNC="$PWD/build/runtime-trueasync/linux/php-trueasync-0.10.0-php8.6-linux-x86_64/php" \
  ./build/linux-amd64/go/bin/go test -v -run '^TestTrueAsyncCompatibility$' .
CGO_ENABLED=0 OOTH_TEST_TRUEASYNC="$PWD/build/runtime-trueasync/linux/php-trueasync-0.10.0-php8.6-linux-x86_64/php" \
  ./build/linux-amd64/go/bin/go test -v -run '^TestTrueAsyncExtraHandle$' .
```

`TestTrueAsyncCompatibility` is opt-in. Its Windows adoption case deliberately
asserts the known stdin failure, so a green compatibility audit alone does **not**
mean a working stdin-based Windows worker. If a runtime upgrade fixes that case, the
test must change to require successful adoption.

## Local results

Verified on Windows amd64 and Linux amd64/WSL2:

| Case | Linux | Windows |
| --- | --- | --- |
| Built-in server, own listener, HTTP/1.1 | 200 response | 200 response |
| Built-in server, own listener, h2c prior knowledge | HTTP/2 200 response | HTTP/2 200 response |
| Imported stdin duplicate, coroutine `socket_accept` | Three connections served | Fails before PHP script runs |
| Built-in server adopting ooth listener | No public adoption entry point found | Same missing API, plus stdin startup failure |
| Extra handle, ordinary stdin, three worker processes | Async accept succeeds on fd 3 | Native HTTP/1.1 and h2c succeed via private FFI hook |
| Extra handle, full ooth lifecycle and native WebSocket | Skipped: static release has no FFI | Pass directly and through Caddy/Nginx |

`socket.php` is a low-level diagnostic using the runtime's socket extension and
coroutines, **not** a replacement HTTP implementation. `server.php` exercises
the real native HTTP server on its own control listener, without activation.
The test parent passes stdin using ooth's `OpenListener`; it never accepts or
forwards the application traffic. Children are forcefully cleaned up by this
bounded diagnostic. The separate `worker.php` lifecycle suite verifies graceful
draining on Windows; do not confuse its result with these older control probes.

## Extra handle and FFI workaround

The user proposed keeping stdin/stdout/stderr available and passing the listener
separately. `TestTrueAsyncExtraHandle` proves this with ooth's `OpenListener` and
three simultaneously running PHP processes. Every child reads an ordinary line
from stdin, writes readiness on stdout and diagnostics on stderr, and must serve
its own PID through the inherited listener. The parent never accepts or forwards
traffic. ooth now defaults to this environment/extra-handle convention, with
explicit `socket_handoff: stdin` for legacy workers. This original three-worker
fixture does not implement the protocol; the newer `worker.php` does.

Existing Go APIs provide the inheritance, without another toolchain patch:

- Linux: `Cmd.ExtraFiles[0]` becomes descriptor **3**, the fourth descriptor after
  0/1/2. Descriptor 4 would be the fifth slot.
- Windows: an inheritable duplicate goes in `SysProcAttr.AdditionalInheritedHandles`.
  The child receives the native handle with that value; Go does not create a
  Windows CRT descriptor numbered 3. The parent's duplicate closes after Start.
- The experimental `OOTH_LISTEN_HANDLE` environment variable conveys the actual
  value. A portable convention can share this variable, but cannot promise the
  same fixed numeric descriptor on both operating systems.

On Windows, the distributed `php_ffi.dll` is enabled with `ffi.enable=1`.
`ffi-server-windows.php` temporarily replaces the exported function pointer
`zend_async_socket_listen_fd_fn` with a PHP FFI callback, saves its original value,
and restores it immediately when called. The callback gives the inherited socket
to the original native adoption function. The real built-in HTTP server then
handles accept, HTTP/1.1, h2c and the coroutine handler. Each of three live workers
has returned its PID over both HTTP versions in local tests.

This **private ABI workaround has limits**:

- It is restricted to the tested Windows 0.10.0 runtime, one configured listener
  and `setWorkers(1)` per process; it is not a public PHP adoption API.
- Before the hook runs, the native server binds an unused temporary loopback
  listener on port 0. The hook closes its adoption duplicate and substitutes the
  inherited handle; the original temporary listener stays owned by the server.
  Actual test requests use only ooth's listener, but this is not a bind-free adapter.
- The reactor takes ownership of the child's inherited socket. The supervisor
  retains its separate handle. The function-pointer callback must remain alive
  during the call, and the saved pointer must be a value copy, not a reference.
- Linux's official binary is static and does not include FFI (`--with-ffi` is
  absent from its build configuration); the 0.10.0 release supplies no alternative
  Linux FFI build. The user chose to record this limit without compiling a runtime.
  Its extra-handle test uses
  `socket.php` and async `socket_accept`, not the built-in HTTP server. No alternative
  extension or interpreter build has been installed.
- The original extra-handle diagnostic uses forceful bounded cleanup. The new
  Windows lifecycle matrix covers telemetry, graceful draining, autoscaling,
  Caddy/Nginx and WebSockets using the same hook. Linux native-server adoption
  remains untested, and neither suite establishes private ABI stability.

The result establishes that a released Windows binary can adopt the listener
without recompilation and that an additional handle avoids the stdin startup
failure. A public native-server adoption method remains the clean implementation.

### Linux fd zero

Direct `socket_import_stream(STDIN)` followed by async `socket_accept` crashed
the released binary. Importing `fopen('php://fd/0', 'r+')` works: this duplicates
stdin to a nonzero descriptor without binding a different socket.
The inspected reactor's `libuv_new_poll_event` tests `socket != 0`, so descriptor
zero falls through without initializing a poll handle. This source finding
matches the experiment; no interpreter patch was applied here.

### Windows startup

With a listening socket on stdin, CLI stream initialization fails before the
first line of the PHP script, with:

```text
Async\AsyncException: Failed to open TCP handle: socket operation on non-socket
```

The inspected PHP `php_stdiop_init_async_io` identifies a native Winsock handle
through `_get_osfhandle(self->fd)`, but passes the CRT descriptor to the reactor's
TCP constructor. The reactor expects a native `SOCKET`. Changing the PHP script
cannot fix a failure that occurs before it runs.

## Required native server extension

Reflection of the released `TrueAsync\HttpServerConfig` and the current server
source exposes host/port and Unix-path listener creation, but no inherited
descriptor/stream adoption method. `TRUE_ASYNC_SERVER_SHARED_LISTEN_FD` only
switches internal sharing of newly bound listeners; it does not import stdin.
Internally, `http_server_class.c` already calls `ZEND_ASYNC_SOCKET_LISTEN_FD` for
duplicates shared between its own worker threads.

The minimal integration should expose that existing primitive, for example a
new **proposed, not currently available** `HttpServerConfig::addInheritedListener(handle)`:

1. Accept a native listener from `OOTH_LISTEN_HANDLE` (fd on Linux, SOCKET on
   Windows), verify it and duplicate it with explicit ownership. Legacy stdin
   retrieval can remain a separate option.
2. Feed the duplicate into `ZEND_ASYNC_SOCKET_LISTEN_FD` without any bind/rebind,
   preserving the native HTTP/1 and HTTP/2 protocol selection and stop path.
3. Keep an inherited listener out of ordinary Windows stdin stream I/O setup,
   avoiding both the CRT/native handle mismatch and competing libuv adoption.
4. Run the existing lifecycle matrix against the public method, including h2c
   reuse, telemetry, stdin-driven draining, idle zero, reactivation and WebSockets.
   Ordinary native-server control tests do not replace these.

This belongs in TrueAsync/PHP, not another ooth or Go toolchain patch. No public
issue/PR was sent and no patched TrueAsync binary is claimed here.

References inspected:

- [Official release](https://github.com/true-async/releases/releases/tag/v0.10.0).
- [Server source at 9e0909c](https://github.com/true-async/server/blob/9e0909c9d96360f2bf3f1c54337cdc488830da16/src/http_server_class.c).
- [PHP stdin initialization at 0f58503](https://github.com/true-async/php-src/blob/0f5850324e405874b898e9a217500109d1896c25/main/streams/plain_wrapper.c).
- [Reactor source](https://github.com/true-async/php-async/blob/master/libuv_reactor.c).

The source audit used those current branches; it is not a claim that their HEAD
commits exactly match every object in the published 0.10.0 binaries.
