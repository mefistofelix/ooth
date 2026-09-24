# Worker lifecycle and WebSocket matrix

`TestRuntimeLifecycle` starts the actual ooth manager for every case. Workers are
started only by socket demand and the occupancy policy, never by the harness.
The same lifecycle runs directly, through Caddy, and through Nginx; ooth also
starts and supervises the proxy. Loopback TCP is used throughout.

This matrix is exclusively for inherited sockets. Ordinary pools whose workers
bind their own listeners have a separate [suite and results](../reuseport/README.md),
`TestWorkerOwnedListeners`; they keep a minimum of one and do not test cold
socket activation or idle zero.

Final local run on 2026-09-24: **30 Windows cases passed; 24 Linux cases passed,
with six explicit TrueAsync skips**. [results.csv](results.csv) saves the result
for each runtime/protocol/route, without machine-specific paths or claiming
performance measurements. The two OS suites ran on the same host; these are
functional checks. Separate PHP-CGI coverage is listed below.

| Runtime | Linux amd64/WSL2 | Windows amd64 | Upstream protocols |
| --- | --- | --- | --- |
| Python standard example | Full lifecycle | Full lifecycle | HTTP/1.1 |
| Node 26.10.0 | Full lifecycle + WebSocket | Full lifecycle + WebSocket | HTTP/1.1, h2c |
| Bun 1.4.2 | Full lifecycle + WebSocket | Full lifecycle + WebSocket, FFI adapter | HTTP/1.1, h2c |
| Deno 2.9.7 | Full lifecycle + WebSocket | Full lifecycle + WebSocket, FFI adapter | HTTP/1.1, h2c |
| Socketify 0.0.31 pinned binary | Full lifecycle + native WebSocket | Full lifecycle + native WebSocket | HTTP/1.1 |
| TrueAsync 0.10.0 | Native-server adoption blocked: distributed binary lacks FFI | Full lifecycle + native WebSocket, FFI hook | HTTP/1.1, h2c |
| Stock PHP-CGI, separate `TestProxyStack` | FastCGI activation | FastCGI activation with launcher | FastCGI, no ooth telemetry |

"Full lifecycle" means a cold response, a hot response, automatic growth to two
ready workers under request occupancy, responses from both PIDs, balanced
start/end events, idle retirement to zero, a fresh PID on reactivation, and a
complete response when shutdown begins during the handler. The test rejects
graceful-timeout kills and retained workers. Resource gating is explicitly
disabled to isolate worker behavior. This is correctness validation, not a
throughput or cold-start benchmark. ARM64 is not executed by this matrix.

## WebSocket coverage

HTTP/1.1 cases for Node, Bun, Deno, Socketify and Windows TrueAsync also require
an actual RFC6455 101 upgrade with the correct accept key, text/binary echo,
ping/pong, continued use beyond the idle TTL, a normal client close, and a
server-initiated 1001 Going Away on stdin stop. Another HTTP response remains
in flight during that final stop and must still complete. The Caddy/Nginx
templates include `/ws`; these test upgrades are HTTP/1.1, not HTTP/2 extended
CONNECT (RFC8441), TLS or compression tests.

Each WebSocket session occupies one telemetry request from open through close.
Messages do not independently consume request slots. Configure concurrency for
long-lived sessions accordingly: one open session at concurrency one can cause
ooth to create spare capacity. Idle spare workers can later retire.

Socketify and TrueAsync use their native WebSocket engines. Node's standard
library has no WebSocket server API: the shared JavaScript worker attaches a
small bounded echo fixture to `node:http`'s upgrade event on all three JS
runtimes. That fixture accepts masked, unfragmented frames up to 125 bytes;
it is not a general WebSocket implementation or a certification of Bun.serve
or Deno.upgradeWebSocket. The Go test client has the same bounded scope.

## Distribution and shutdown details

A shared listener distributes new connections, without promising round-robin
accept fairness. Existing HTTP/2 streams remain on their original connection.
The lifecycle Caddy template opts into fresh upstream h2c connections so that
new requests can reach new capacity; the earlier pressure suite still retains
sessions to measure that limitation. The JavaScript fixture also rotates h2c
sessions. Socketify's growth phase deliberately blocks its first event loop for
600 ms (`/busy600`), proving that the new worker can accept pending connections;
ordinary handlers and the shutdown probe use asynchronous delay. These choices
are test workloads, not proposed production scheduling or pooling policies.
The harness sends an additional connection as soon as the second worker is ready,
while the first workload is still active; waiting for all original replies would
remove the pressure before checking whether the new capacity serves traffic.
See [FINDINGS.md](FINDINGS.md) for the observed cases, causes and remaining limits.

The proxy is a dependency of the worker fixture so it remains available until
worker responses drain. The JS adapters ignore peer ECONNRESET/EPIPE as
connection failures rather than killing their listener. Deno's injected HTTP/1
connection is explicitly drained/closed after its response; adapter exit waits
for close callbacks and stdout flushing. Socketify closes only its listener
first and leaves its loop running until handlers finish. Windows TrueAsync uses
its public `stop()` and explicit WebSocket `recv()` loop. The native adoption
methods remain the pinned, experimental adapters documented in their own probe
directories; no runtime is recompiled and no Go dependency is added.

## Reproduce

Prepare the dedicated Go compiler and the existing portable runtimes using the
[proxy](../proxy/README.md), [JavaScript](../javascript/README.md),
[Socketify](../socketify/README.md), and [TrueAsync](../trueasync/README.md)
instructions. Set these environment variables to absolute executable paths:

```text
OOTH_TEST_PYTHON
OOTH_TEST_NODE
OOTH_TEST_BUN
OOTH_TEST_DENO
OOTH_TEST_SOCKETIFY
OOTH_TEST_TRUEASYNC
OOTH_TEST_CADDY
OOTH_TEST_NGINX
```

Socketify also needs the documented `PYTHONPATH` and Linux `LD_LIBRARY_PATH`.
The worker inherits them; install nothing globally. Unset runtime variables
produce explicit skips. TrueAsync Linux is an explicit unsupported skip even
when its executable is supplied: the user chose to retain the released binary
without rebuilding it. Its existing async-accept probe remains available.

```powershell
$env:CGO_ENABLED = '0'
$env:OOTH_TEST_ARTIFACTS = "$PWD/build/runtime-matrix-windows"
./build/windows-amd64/go/bin/go.exe test -v -count=1 -run '^TestRuntimeLifecycle$' -timeout 240s .
```

```sh
export CGO_ENABLED=0
export OOTH_TEST_ARTIFACTS="$PWD/build/runtime-matrix-linux"
./build/linux-amd64/go/bin/go test -v -count=1 -run '^TestRuntimeLifecycle$' -timeout 240s .
```

Run Linux inside a writable delegated cgroup, or use `with-cgroup.sh` as described
in the proxy suite. Without delegation ooth warns and supervises direct children
only. `OOTH_TEST_ARTIFACTS` retains each actual YAML, proxy configuration and
`ooth.log` under ignored build/. All worker source, template and assertion code
is versioned. GitHub builds remain manual-only.
