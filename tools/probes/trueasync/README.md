# TrueAsync 0.10.0 compatibility audit

The requested target is TrueAsync's built-in HTTP/1.1 + HTTP/2 server adopting
ooth's listener on stdin, one PHP thread with coroutines, request telemetry and
graceful shutdown. **This integration is not complete with the released binary.**
Do not use the independent control listener as a replacement for activation.

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
```

Linux:

```sh
bash tools/probes/trueasync/prepare-linux.sh
CGO_ENABLED=0 OOTH_TEST_TRUEASYNC="$PWD/build/runtime-trueasync/linux/php-trueasync-0.10.0-php8.6-linux-x86_64/php" \
  ./build/linux-amd64/go/bin/go test -v -run '^TestTrueAsyncCompatibility$' .
```

`TestTrueAsyncCompatibility` is opt-in. Its Windows adoption case deliberately
asserts the known failure, so a green compatibility audit does **not** mean a
working activated Windows worker. If a runtime upgrade fixes that case, the
test must change to require successful adoption.

## Local results

Verified on Windows amd64 and Linux amd64/WSL2:

| Case | Linux | Windows |
| --- | --- | --- |
| Built-in server, own listener, HTTP/1.1 | 200 response | 200 response |
| Built-in server, own listener, h2c prior knowledge | HTTP/2 200 response | HTTP/2 200 response |
| Imported stdin duplicate, coroutine `socket_accept` | Three connections served | Fails before PHP script runs |
| Built-in server adopting ooth listener | No public adoption entry point found | Same missing API, plus stdin startup failure |

`socket.php` is a low-level diagnostic using the runtime's socket extension and
coroutines, **not** a replacement HTTP implementation. `server.php` exercises
the real native HTTP server on its own control listener, without activation.
The test parent passes stdin using ooth's `OpenListener`; it never accepts or
forwards the application traffic. Children are forcefully cleaned up by this
bounded diagnostic; graceful draining has not yet been certified for TrueAsync.

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
new **proposed, not currently available** `HttpServerConfig::addStdinListener()`:

1. Resolve stdin to an fd on Linux or `GetStdHandle(STD_INPUT_HANDLE)` on Windows,
   verify a listening socket, and duplicate it with explicit ownership.
2. Feed the duplicate into `ZEND_ASYNC_SOCKET_LISTEN_FD` without any bind/rebind,
   preserving the native HTTP/1 and HTTP/2 protocol selection and stop path.
3. Keep an inherited listener out of ordinary Windows stdin stream I/O setup,
   avoiding both the CRT/native handle mismatch and competing libuv adoption.
4. Test multiple ooth worker processes sharing that listener, h2c reuse and
   concurrency, request telemetry, signal-driven draining, idle zero and
   reactivation. Ordinary native-server control tests do not replace these.

This belongs in TrueAsync/PHP, not another ooth or Go toolchain patch. No public
issue/PR was sent and no patched TrueAsync binary is claimed here.

References inspected:

- [Official release](https://github.com/true-async/releases/releases/tag/v0.10.0).
- [Server source at 9e0909c](https://github.com/true-async/server/blob/9e0909c9d96360f2bf3f1c54337cdc488830da16/src/http_server_class.c).
- [PHP stdin initialization at 0f58503](https://github.com/true-async/php-src/blob/0f5850324e405874b898e9a217500109d1896c25/main/streams/plain_wrapper.c).
- [Reactor source](https://github.com/true-async/php-async/blob/master/libuv_reactor.c).

The source audit used those current branches; it is not a claim that their HEAD
commits exactly match every object in the published 0.10.0 binaries.
