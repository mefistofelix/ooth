# JavaScript inherited listener probes

`TestJavaScriptExtraHandle` and `worker.cjs` use the same `OOTH_LISTEN_HANDLE`
convention as ooth: a real inherited fd on Linux or native SOCKET on Windows.
The environment only communicates its number. stdin is a separate pipe.
The Go parent never accepts or forwards application traffic.

Locally verified on amd64 Linux/WSL2 and Windows:

| Runtime | Linux | Windows | HTTP server path |
| --- | --- | --- | --- |
| Node 26.10.0 | Pass | Pass | `node:http`, public fd on Linux; private TCPWrap on Windows |
| Bun 1.4.2 | Pass | Pass with FFI | Linux `node:net` fd listener; Windows Winsock accept thread, both feed `node:http` |
| Deno 2.9.7 | Pass | Pass with FFI | `node:http` compatibility API; Windows Winsock accept plus connected-socket adoption |

Each positive case starts three workers concurrently on one listener and checks
a complete HTTP/1.1 response body identifying every new worker. Workers read an
ordinary stdin line before announcing readiness. The test then sends the
line-based stop command, checks clean process exits, verifies survivors still
accept, and stops the last worker with a response body pending to check draining.
This original extra-handle probe covers HTTP/1.1 and process handoff only.
The newer [TestRuntimeLifecycle matrix](../runtime/README.md) uses the shared
`../node_worker.cjs` with Node, Bun and Deno on both OSes: actual ooth activation,
telemetry, growth, idle zero, reactivation and active-request draining, over
HTTP/1.1 and h2c, directly and through Caddy/Nginx. Its HTTP/1.1 cases also check
WebSocket upgrade, text/binary echo, ping/pong and both close directions.
The JS WebSocket echo is a bounded test fixture on the HTTP upgrade event, not
a native Bun.serve/Deno.upgradeWebSocket test or a general WebSocket library.
Unix sockets and peak performance remain outside these fixtures.

The separate `DENO_DIRECT` case preserves the **known negative result** for
direct listener import. Public `http.Server.listen({fd: nativeHandle})` also failed in
the initial probe, terminating the runtime with `0xc0000409`; that fatal probe
is not run in the regular suite. The saved test calls private TCPWrap directly
and checks its controlled `-4071` error. This is a pinned-version expectation;
revisit it when changing Deno.

## Why the adapters differ

Node's extra-handle path no longer needs FFI: `listener.cjs` reads the native
number from the environment and calls its private TCPWrap binding. FFI remains
only for the older, explicit stdin experiment. The separate `TestNodeWorker`
and the common runtime matrix exercise telemetry, cold activation, scaling,
h2c, idle shutdown and active-request draining through the supervisor. The
shared worker treats accepted-connection resets as peer failures, explicitly
drains/closes Deno's injected HTTP/1.1 connection, and lets close callbacks and
stdout flush before adapter exit. See the [stack findings](../runtime/FINDINGS.md).

Bun's `http.Server.listen({fd})` does not adopt the supplied listener: its
implementation constructs `Bun.serve` options from address/port/path, so the
initial probe listened on an unrelated port. `node:net` does implement fd
adoption through `Bun.listen`. Bun's HTTP compatibility server explicitly
supports `server.emit('connection', socket)`, allowing the worker to accept
through `node:net` and use the runtime's existing HTTP parser and server.
On Windows this direct TCP-listener path served responses but intermittently
stopped processing stdin commands, even after explicitly setting FIONBIO.
The root cause inside Bun is not isolated; do not describe FIONBIO alone as its
fix. The direct experiment remains reproducible by setting `TEST_BUN_DIRECT=1`
and running the Bun case repeatedly; it can fail at the bounded stop assertion.

The working Windows adapter in `bun-windows.cjs` uses `bun:ffi` inside a
`worker_threads.Worker` in the same process: infinite WSAPoll waits, then
nonblocking Winsock accept. It posts accepted handle numbers to the main
thread, which adopts them with `net.Socket.connect({fd, fdIsRawSocket: true})`
and hands them to the HTTP compatibility server. It installs the HTTP handler
before starting socket reads. The raw-handle adoption option is internal;
this is still an experimental adapter. Bun and Deno FFI paths passed five
consecutive three-process/drain runs on Windows after the final changes.
There is no handwritten HTTP parser and no application proxy. This does not
establish that the native `Bun.serve` API can adopt a listener.

Source inspected at Bun commit
[`744846f`](https://github.com/oven-sh/bun/tree/744846f844374847c902b5e7fd59b4342a51ef99):
[`_http_server.ts`](https://github.com/oven-sh/bun/blob/744846f844374847c902b5e7fd59b4342a51ef99/src/js/node/_http_server.ts),
[`net.ts`](https://github.com/oven-sh/bun/blob/744846f844374847c902b5e7fd59b4342a51ef99/src/js/node/net.ts).
Deno commit
[`0c07124`](https://github.com/denoland/deno/tree/0c071246a412575e07423263404a5d13e7ed6aa2)
has a Windows CRT-fd conversion in
[`util.rs`](https://github.com/denoland/deno/blob/0c071246a412575e07423263404a5d13e7ed6aa2/ext/node/ops/util.rs)
and a platform-specific TCP open path in
[`tcp_wrap.rs`](https://github.com/denoland/deno/blob/0c071246a412575e07423263404a5d13e7ed6aa2/ext/node/ops/tcp_wrap.rs).
The Windows path wraps the listener as a connected Tokio `TcpStream`; the
listener-specific import in
[`uv_compat/tcp.rs`](https://github.com/denoland/deno/blob/0c071246a412575e07423263404a5d13e7ed6aa2/libs/core/uv_compat/tcp.rs)
is Unix-only. The tested workaround in `deno-windows.cjs` calls WSAStartup,
waits through WSAPoll via nonblocking Deno FFI, and calls Winsock accept inside
the worker. It explicitly sets FIONBIO so an accept race with another worker
returns WSAEWOULDBLOCK instead of blocking the JS thread. It passes each
accepted socket through private TCPWrap into
`net.Socket` and emits `connection` on the runtime's HTTP server. Deno then
handles connection I/O and HTTP parsing normally. The parent still never accepts.

Both Windows FFI adapters wait indefinitely: Deno uses its FFI pool, Bun a
dedicated worker thread. There is no periodic readiness timer. Each consumes
a blocking native thread while idle and is not a proposed
high-performance final API. The fixture closes its listener on stdin stop,
drains accepted connections, then exits explicitly. It keeps Winsock/the DLL
loaded until exit because an FFI wait can still be returning after closesocket;
this does not certify safe adapter unloading inside a long-lived runtime.
The shim is restricted to the tested Windows amd64 ABI and native handles
representable by Deno's signed 32-bit TCPWrap API. No runtime patch or native
library was compiled. A proper Windows listener adoption API remains preferable.
`TEST_JS_TRACE=1` adds shutdown diagnostics on stderr.

## Reproduce

Prepare ooth's dedicated compiler with `build.sh`. `prepare.py` downloads the
official Bun/Deno release ZIPs for amd64, verifies the recorded GitHub release
asset SHA256 digests in `archives.json`, and extracts only under ignored
`build/runtime-js`. No global install or application dependency is added.
Supply Node from the existing Node probe preparation or a compatible runtime.

```powershell
python tools/probes/javascript/prepare.py windows
$env:CGO_ENABLED = '0'
$env:OOTH_TEST_NODE = "$PWD/build/runtime-node/node-v26.10.0-win-x64/node.exe"
$env:OOTH_TEST_BUN = "$PWD/build/runtime-js/bun-windows-x64/bun-windows-x64/bun.exe"
$env:OOTH_TEST_DENO = "$PWD/build/runtime-js/deno-x86_64-pc-windows-msvc/deno.exe"
./build/windows-amd64/go/bin/go.exe test -v -count=1 -run 'Test(JavaScriptExtraHandle|NodeWorker|WorkerStopTransport|SocketHandoffConfig)' -timeout 90s .
```

```sh
python3 tools/probes/javascript/prepare.py linux
export CGO_ENABLED=0
export OOTH_TEST_NODE="$PWD/build/runtime-node/node-v26.10.0-linux-x64/bin/node"
export OOTH_TEST_BUN="$PWD/build/runtime-js/bun-linux-x64/bun-linux-x64/bun"
export OOTH_TEST_DENO="$PWD/build/runtime-js/deno-x86_64-unknown-linux-gnu/deno"
./build/linux-amd64/go/bin/go test -v -count=1 -run 'Test(JavaScriptExtraHandle|NodeWorker|WorkerStopTransport|SocketHandoffConfig)' -timeout 90s .
```

`TestWorkerStopTransport` separately verifies ooth itself: extra listener and
virtual service stdin commands, first-line handshake gating, legacy stdin
signal fallback, ignored command followed by forced termination, and Linux fd 7.
These are local opt-in runtime probes; GitHub builds are not dispatched.
