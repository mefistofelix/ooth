# Runtime and proxy findings

Recorded on 2026-09-24. These observations cover Windows amd64 and Ubuntu 24.04
under WSL2 on the same development machine, using Go 1.27.1, Node 26.10.0,
Bun 1.4.2, Deno 2.9.7, Socketify 0.0.31's pinned native binaries, TrueAsync 0.10.0,
Caddy 2.11.4 and Nginx 1.30.5. Python is 3.14.3 on Windows and 3.12.3 on Linux.
They do not establish behavior on every version, machine or architecture.

The [matrix and reproduction instructions](README.md) define what is asserted.
`TestRuntimeLifecycle` runs ooth itself; the older `*ExtraHandle` tests isolate
runtime adoption with a small parent harness. Their results must not be confused.
Sources, templates and assertions are committed. Generated YAML, proxy configs
and `ooth.log` are retained with `OOTH_TEST_ARTIFACTS` under ignored `build/`.

## Connection distribution

### D1: Caddy h2c can keep all work on one worker

**Observed:** the earlier pressure suite prestarted four blocking Node workers.
Caddy still used one serving PID on both OSes. Nginx used two on Windows and four
on Linux. Caddy's asynchronous handler variant also used one worker but did not
develop the same queue. The [pressure results](../pressure/RESULTS.md) and
committed CSVs preserve both repetitions, delays and PID counts.

**Established mechanism:** HTTP/2 streams on an existing connection stay with
the worker that accepted that connection. Adding processes cannot move them.
These measurements do not establish a universal Nginx fairness rule or explain
every OS difference in accept distribution. The pressure measurements used the
earlier scaling policy, not today's mean-occupancy replacement.

**Lifecycle fixture:** Windows TrueAsync through Caddy/h2c initially grew a
second ready worker without routing requests to it. Setting Caddy's upstream
`keepalive off` for the new lifecycle matrix made fresh connections reach the
new capacity. The shared JS lifecycle worker also retires h2c sessions with
GOAWAY. These are deliberate test conditions, not a recommended production
pooling policy. The separate pressure fixture retains sessions to expose this
blind spot. No throughput improvement is claimed.

### D2: Shared accept is atomic, not round-robin

**Observed:** Windows Socketify through Caddy repeatedly served the original
worker when the test waited for all blocking requests to finish before making
its distribution probes. Up to 104 later requests still hit that PID; the spare
could reach its idle timeout. The same test did not fail on the other routes.

**Test correction:** connect immediately when the second worker becomes ready,
while `/busy600` still occupies the original event loop. Three consecutive
Windows Caddy repetitions then passed with both PIDs serving. The test also
allows concurrent follow-up connections instead of assuming fair serial accepts.
This verifies useful additional capacity under that workload, not a fix to a
kernel/runtime scheduling bug. The precise preference between two idle acceptors
has not been isolated.

**General constraint:** one successful accept removes one connection from the
shared queue. A readiness notification reserves nothing. Multiple observers can
wake for one connection; a later accept must handle EAGAIN/WOULD-BLOCK. Both
Windows FFI adapters set FIONBIO and return to waiting on WSAEWOULDBLOCK (10035).
ooth observes the listener but never competes by accepting application traffic.

### D3: Listener notifications do not count waiting requests

The pressure suite found frequent notifications with negligible waiting and
long waits without notifications. Accept can precede ooth's observation, so a
next-accept pairing can accidentally match a later connection. Internal handler
queues and persistent h2c streams are not visible through listener readiness.
Exact arrival/queue timestamps are not part of the production protocol. Current
growth uses start/end occupancy with declared concurrency; CPU/memory limits
only gate optional growth. See the pressure report before proposing a different
queue-pressure heuristic.

## Runtime adoption and shutdown

| Case | Observed result and current handling | Reproduction / limit |
| --- | --- | --- |
| Windows Node | Extra native handle enters private TCPWrap; ordinary stdin remains available. Closing readline alone left its pipe alive, so the worker also destroys stdin. | `TestNodeWorker`, `TestJavaScriptExtraHandle`, common matrix. Private Windows API; Linux uses public fd adoption. |
| Windows Bun direct listener | Responses worked, but stdin stop intermittently stalled even with FIONBIO. | `TEST_BUN_DIRECT=1` retains the diagnostic. Root cause remains unresolved. |
| Windows Bun FFI | A thread in the same process waits in WSAPoll, accepts nonblocking sockets, and transfers handles to the HTTP compatibility server before starting reads. | Three-worker test and full matrix pass. Internal `fdIsRawSocket` option; native `Bun.serve` adoption is not established. |
| Windows Deno direct listener | Private direct import returns UV_EINVAL; an earlier public call terminated the runtime. | `DENO_DIRECT` is a known-negative compatibility assertion, not a supported worker. |
| Windows Deno FFI | Winsock accepts on an FFI pool thread; private TCPWrap adopts each connected socket. | Three-worker test and full matrix pass. Linux uses the fd listener path. |
| JS peer reset | ECONNRESET/EPIPE from an accepted connection could become a fatal listener error; seen while retiring h2c connections. | Adapters now treat these as connection failures. Other errors retain their error path. |
| Deno HTTP/1.1 drain | An injected connection could remain alive after the response and delay process exit. | After response finish, `destroySoon()` drains and closes that connection. This fixture does not validate persistent HTTP/1.1 pooling. |
| JS adapter exit | Listener close can precede connection-close telemetry; lingering wait infrastructure can retain the process. | Exit is deferred until close callbacks and stdout flushing after listener/response drain. Safe adapter unload inside a long-lived process is not established. |
| Socketify | Native HTTP/WS work through a ctypes poll reinitialization. Stopping the whole app too early prevents active handlers finishing. | Protocol mode closes the listener first and stops the loop after active work ends. SHA256-pinned private layout; no native rebuild. |
| TrueAsync Windows stdin | A listener on stdin fails before the PHP script runs, at stream initialization. | Historical compatibility probe checks this failure. Extra inherited handle avoids it; no ooth handle correction is needed. |
| TrueAsync Windows extra handle | Private FFI hook connects the listener to the built-in HTTP/1.1, h2c and WS server. | Common matrix covers ooth lifecycle, proxies and graceful stop with one server thread. A temporary loopback listener is still created by the native server. |
| TrueAsync WebSocket iteration | An early iterator-based handler stalled during the WS exchange. Explicit `recv()` passed subsequent upgrade/echo/ping/close probes. | Current worker uses `recv()`. This does not prove a native iterator bug; the root cause of the earlier stall is not isolated. |
| TrueAsync Linux | Released binary is static, has no FFI, and its build configuration lacks `--with-ffi`; no alternate Linux FFI asset was found in 0.10.0. | Async accept on an extra fd works. Native HTTP-server adoption is explicitly skipped. User requested no recompilation. |
| Stock PHP-CGI | Uses listener on stdin, with no ooth handshake. Windows additionally requires invalid stdout/stderr handles for FastCGI detection. | `TestProxyStack` uses the explicit Windows launcher. Activation and supervision pass; no request scaling, idle-zero, WS or Windows active-request graceful-drain claim. |

Implementation details and pinned source references are in the
[JavaScript](../javascript/README.md), [Socketify](../socketify/README.md),
[TrueAsync](../trueasync/README.md) and [PHP-CGI](../proxy/README.md) notes.
No runtime was recompiled for this matrix and no Go dependency was added.

## WebSocket and test-harness details

- HTTP/1.1 upgrades pass directly, through Caddy and through Nginx. Caddy's host
  routing initially returned a normal empty 200 when the test sent `Host:
  localhost` to a site configured for its numeric address. Using the actual
  address fixed the test; this was not a WebSocket adoption failure.
- Socketify and TrueAsync use native WebSocket servers. The shared JS fixture
  uses `node:http`'s upgrade event and a small echo implementation. Tests cover
  masked unfragmented frames up to 125 bytes: text, binary, ping/pong, normal
  client close and server 1001 close on stop. They do not cover fragmentation,
  compression, TLS or HTTP/2 extended CONNECT (RFC8441).
- A WS session reports one active request from open to close. It survives the
  idle TTL and ends its event pair on closure. With concurrency one, a long-lived
  session can trigger spare capacity even between messages; that follows the
  declared capacity model. Configure concurrency/request timeout accordingly.
- The final stop probe keeps a WS session and an HTTP response active together.
  Both must finish before process exit; the test rejects a graceful-timeout kill
  or unbalanced request events. Proxies are dependencies so they remain running
  until worker draining finishes.
- The plain Python sample is a small HTTP/1.1 worker, not Socketify and not a
  native HTTP/2 server. Python's native HTTP/WS coverage here comes from Socketify.

Cold/hot responses are correctness checks. These logs are not cold-start or
throughput benchmarks, and the private adapters are not production stability
certifications. TCP loopback is the shared matrix; separate Go-worker tests
cover Unix listeners and permissions. ARM64 remains cross-compile-only.

## Workers binding their own TCP sockets: separate suite

The [worker-owned listener suite](../reuseport/README.md) has its own test entry
point, results and artifacts. It does not replace this inherited-socket matrix.
Linux passed 27 cases with minimum one, growth, two serving PIDs, idle one,
crash restart and active drain. The following distinctions matter for the stack:

- SO_REUSEPORT groups independently bound listeners with separate accept queues.
  The saved `queue.py` probe connects before starting the second listener; that
  connection stays on the first. New connections reach both queues. Leaving a
  supervisor listener open without accepting can therefore strand more clients
  than just the initial activation request. No kernel migration was enabled.
- ooth receives no listener events in these apps: there is no `listen` setting.
  Minimum processes start proactively and telemetry drives growth. The runtime
  enables reuseport and binds; ooth adds no dedicated mode. Request/session
  pinning still applies after connections have been distributed.
- Bun.serve and Deno.serve now have native HTTP/WS coverage in this separate
  Linux suite. Deno's public reusePort option needs `--unstable-net`; the first
  probe failed at that explicit runtime gate, before serving. Deno h2c passed.
- Socketify uses public listen and Linux uSockets reuseport here, not ctypes
  adoption. TrueAsync Linux uses public addListener, which permits testing the
  native HTTP/1.1, h2c and WS server without FFI or any runtime compilation.
  The official binary's inherited-server adoption limit remains unchanged.
- The initial Node crash check killed the minimum immediately after receiving
  a response, before its end event reached ooth. Waiting for that event before
  the intentional idle crash fixed the harness's event-balance assertion;
  response arrival and stdout delivery are independent streams. A real crash
  can of course interrupt an active request; this test does not promise otherwise.
- Windows skips this native reuseport matrix, rather than treating SO_REUSEADDR
  as equivalent. Generic no-listener process pools, placeholders and restart
  still have native Windows tests. Inherited workers remain separately verified
  on Windows, with all 30 existing cases passing after the fixture changes.
