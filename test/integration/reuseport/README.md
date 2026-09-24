# Worker-owned TCP listener tests

This suite is separate from the [inherited-socket matrix](../runtime/README.md):
`TestWorkerOwnedListeners` uses apps **without `listen`**, with `min_workers: 1`.
ooth starts ordinary processes and handles their telemetry, restarts and stop
commands. Each worker creates its own TCP listener with reuseport. There is no
reuseport flag, network proxy or special runtime branch in ooth.

## Results

On 2026-09-24, **27 Linux amd64/WSL2 cases passed**, recorded individually in
[results.csv](results.csv). Every combination below runs directly, through
Caddy 2.11.4, and through Nginx 1.30.5. ooth also starts and supervises the proxy.

| Runtime | Worker API | Tested protocols | HTTP/1.1 WebSocket |
| --- | --- | --- | --- |
| Python 3.12 | stdlib `HTTPServer`, SO_REUSEPORT before bind | HTTP/1.1 | Not implemented in this fixture |
| Node 26.10.0 | `node:http`/`node:http2`, `listen({reusePort:true})` | HTTP/1.1, h2c | Bounded upgrade echo |
| Bun 1.4.2 | Native `Bun.serve({reusePort:true})` | HTTP/1.1 | Native engine |
| Deno 2.9.7 | Native `Deno.serve({reusePort:true})`, `--unstable-net` | HTTP/1.1, h2c | Native `upgradeWebSocket` |
| Socketify 0.0.31 | Public `App.listen`, Linux uSockets default reuseport | HTTP/1.1 | Native engine |
| TrueAsync 0.10.0 / PHP 8.6 | Public `HttpServerConfig.addListener`, one server thread | HTTP/1.1, h2c | Native engine |

**TrueAsync Linux needs neither FFI nor recompilation here.** It creates its own
listener, so the missing public listener-adoption API is not involved. Likewise,
Socketify's native `listen` path does not use the ctypes adoption workaround.
Bun and Deno use their native HTTP servers here, unlike their inherited-socket
compatibility adapters. No additional application dependency was installed.

Each case verifies:

- The configured minimum starts without an incoming connection or inherited fd.
- Two ready workers appear under request occupancy, and both PIDs serve traffic.
- Start/end events balance; idle retirement stops at one existing worker.
- Killing the retained worker causes a replacement through the minimum policy.
- Stop on stdin drains an active HTTP request, with no forced-timeout kill.
- Where listed, WebSocket upgrade, text/binary echo, ping/pong, survival beyond
  idle TTL, normal client close and server 1001 close during HTTP draining.

These are functional tests, not cold-start, throughput or fairness benchmarks.
TCP loopback only; no TLS, Unix sockets or HTTP/2 extended CONNECT. Request
completion events describe handler completion, not remote delivery/acknowledgment
of every response byte. The client separately verifies a complete response.
The h2c direct client asserts HTTP/2; proxy fixtures select h2c upstream transport.

The generic no-listener pool, including growth, idle minimum and stdin stop,
also has `TestPoolWithoutListener` on **both Windows and Linux**, without an
external runtime or network listener. Template argv/environment and crash
restart are checked on both OSes by `TestLaunchTemplatesAcrossRestart`.
This does not claim Windows supports the Linux native reuseport matrix.

## OS and configuration limits

The native multi-listener suite explicitly skips Windows. Node's
[reusePort documentation](https://github.com/nodejs/node/blob/main/doc/api/net.md),
[Bun's cluster guide](https://bun.sh/guides/http/cluster) and
[Deno's TCP options](https://docs.deno.com/api/deno/~/Deno.TcpListenOptions.reusePort)
do not provide this TCP reuseport behavior there. Bun/Deno document Linux support
and ignoring the option on unsupported platforms. Socketify's included uSockets
backend uses SO_REUSEADDR on Windows; it is not the Linux SO_REUSEPORT mechanism.
See Microsoft's [SO_REUSEADDR description](https://learn.microsoft.com/en-us/windows/win32/winsock/using-so-reuseaddr-and-so-exclusiveaddruse).
The existing inherited-listener tests remain the demonstrated multi-worker path
on Windows. A single program can still open its own listener there.

Stock PHP-CGI is not part of this telemetry/reuseport matrix: its tested scalable
alternative here is TrueAsync. PHP-CGI's inherited-stdin FastCGI coverage stays in
`TestProxyStack`, without claims of occupancy-based growth.

No `listen` means no parent socket and no network-driven activation from zero.
The configuration must keep a minimum worker when clients need to reach the
service independently. `min_workers` never selects how the socket is created:
with `listen`, even a minimum of one still receives ooth's listener. With multiple
processes binding, the runtime/OS must support it; on Linux TCP reuseport the
sockets must also have compatible binding options and the same effective UID.
Filesystem Unix paths cannot be treated as a generic reuseport group.

## Separate accept queues: reproduction

```sh
python3 test/integration/reuseport/queue.py
```

The saved pure-stdlib diagnostic first connects to listener A, observes it
readable, then starts process B binding the same TCP port with SO_REUSEPORT.
B remains unreadable; A still accepts that original connection. A second phase
connects 32 new clients while both listeners exist and finds connections in
**both independent queues**. One local run split them 13/19; the counts are not
an expected ratio. These diagnostic processes accept only to identify the
queues; ooth itself never accepts or forwards traffic.

The [Linux kernel documentation](https://docs.kernel.org/networking/ip-sysctl.html#tcp-migrate-req-boolean)
describes connection/listener association and the separate, default-disabled
`tcp_migrate_req` mechanism for listener closure. This test changes no sysctl
and installs no BPF program. Leaving an inactive parent in a reuseport group
can strand later clients too; spawning another listener is not socket activation.

## Reproduce the lifecycle matrix

Use the portable runtime preparation in [proxy](../proxy/README.md),
[JavaScript](../javascript/README.md), [Socketify](../socketify/README.md) and
[TrueAsync](../trueasync/README.md). Set the same absolute executable variables:

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

Socketify additionally needs its documented PYTHONPATH and LD_LIBRARY_PATH.
The test supplies Deno's `--unstable-net`; TrueAsync Linux uses its official
binary with `-n`, no FFI extension. Missing executables produce explicit skips.

```sh
export CGO_ENABLED=0
export OOTH_TEST_ARTIFACTS="$PWD/tmp/worker-owned-linux"
./build/linux-amd64/go/bin/go run ./test/run.go test -v -count=1 \
  -run '^TestWorkerOwnedListeners$' -timeout 240s .
```

Use a writable delegated cgroup, or `with-cgroup.sh` as in the other probe suites.
Actual YAML, rendered Caddy/Nginx configuration and ooth logs are saved below
that separate ignored artifact directory. Results are versioned here, not in
the inherited-listener CSV. Common lifecycle assertions and proxy templates
are reused; the test entry point, cases, configs, output and interpretation
remain distinct. The proxy is a dependency so it stays up during worker drain.

The standalone [ooth.yaml](ooth.yaml) and [node.yaml](node.yaml) demonstrate a
Node pool; ensure `node` names the tested runtime in PATH. `vars.endpoint` is a
literal shared value expanded into the worker's environment. Python's fixture
instead receives `{{.vars.endpoint}}` as an argv element. The matrix exercises
both ways of configuring a worker. Changing the URL is the user's decision;
ooth does not interpret that `vars` value or decide whether to enable reuseport.

The proxy fixtures disable upstream keepalive for lifecycle assertions, avoiding
the existing h2c session pinning documented in [FINDINGS.md](../runtime/FINDINGS.md).
Reuseport distributes new connections, not requests already multiplexed on an
existing connection. Socketify uses the same deliberate 600 ms busy-loop phase
as the inherited matrix; regular handlers and draining use asynchronous delays.
The crash test waits for stdout completion telemetry before killing an otherwise
idle worker, so it does not count an intentionally interrupted event as a drain
failure. The supervisor still discards a crashed process's in-flight accounting.
