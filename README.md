# ooth

Minimal process supervisor with lazy socket activation, written in Go. The supervisor lives in **`main.go`**, with process ownership utilities in `job_linux.go` and `job_windows.go`, selected by Go at compilation. Linux and Windows builds use `CGO_ENABLED=0`; the executable does not need Go, a shell, or external management commands at runtime. Windows still uses the operating system's DLLs.

ooth owns TCP or Unix listening sockets. Incoming connections wake a worker pool; each worker inherits the listener on standard input and accepts connections itself. **ooth never accepts, reads, copies, or proxies application traffic.** “Copyless” here means no forwarding through the supervisor, not that the operating system or application performs no copies.

This is an experimental nucleus, not a replacement for all of systemd. It supervises foreground processes, orders dependencies, restarts failed workers with backoff, grows busy pools, and removes idle workers. Windows Jobs and Linux cgroup v2 keep descendants associated with their worker even when intermediate parents exit. Linux adopts and reaps orphans when cgroup supervision is available, and always reaps as PID 1. It does not mount filesystems, configure the machine, or implement a service manager for Windows SCM.

## Build and run

```sh
bash ./build.sh
./bin/ooth-linux-amd64 -check -config examples/ooth.yaml
./bin/ooth-linux-amd64 -config examples/ooth.yaml
curl http://127.0.0.1:8080/
```

On Windows use Git Bash to build, then run `bin/ooth-windows-amd64.exe`. Set the example's `command` to the absolute path of your Python interpreter if `python3` is not available. Production workers may use any language; Python is only an example.

The script downloads and verifies **Go 1.27.1**, patches its dedicated copy under `build/`, runs tests and `go vet`, then builds amd64 and arm64 binaries for the host OS. It never patches an existing Go installation. When the patch changes, affected source files are restored from the verified archive before applying it. The Go version is deliberately pinned: toolchain changes must be reviewed when upgrading Go. A stock `go build` is not supported.

`-version` prints the commit/build version; `-debug` includes completed request metrics. Development changes are tested locally; pushes do not trigger GitHub builds. Run the workflow manually at agreed milestones to verify Linux and Windows on clean runners. Enable `publish_release` when that run should also publish a prerelease containing the four binaries and `SHA256SUMS`; otherwise it only builds/tests and uploads workflow artifacts. ARM64 is cross-compiled, while integration tests run on amd64.

## Configuration

Main `ooth.yaml`:

```yaml
watch:
  - /var/www/app*/ooth.yaml
```

Windows paths may use forward slashes, for example `C:/www/app*/ooth.yaml`. Patterns follow Go's `filepath.Glob`: `*` matches within one directory level; recursive `**` is not supported. Relative paths resolve against the YAML file that contains them. Changes and new matching files are watched with [sgtdi/fswatcher](https://github.com/sgtdi/fswatcher); a five-second rescan also reconciles missed events. YAML uses [goccy/go-yaml](https://github.com/goccy/go-yaml), with unknown fields rejected.

On Linux, an optional main-config `cgroup: /sys/fs/cgroup/my-delegation` selects a writable cgroup v2 subtree. Otherwise ooth uses `OOTH_CGROUP_ROOT`, then its current cgroup under `/sys/fs/cgroup`. This setting is ignored on Windows and requires an ooth restart to change. Missing support or permissions produces a warning on stderr and falls back to direct-child supervision; see descendant ownership below.

Application file:

```yaml
name: web
command: [/usr/bin/python3, worker.py]
directory: .
env:
  APP_MODE: production
listen:
  network: tcp
  address: 127.0.0.1:8080
requires: [database, cache]
min_workers: 0
max_workers: 4
concurrency: 1
idle_timeout: 1m
start_timeout: 10s
stop_timeout: 10s
request_timeout: 0s
scale_delay: 100ms
```

`command` is an argument list, executed directly without a shell. Workers must remain in the foreground. `name` defaults to the containing directory's name. Networks are `tcp`, `tcp4`, `tcp6`, and `unix`. On Windows, the worker's own language/runtime must also support Unix sockets; CPython's Windows build does not currently implement their accept path, so the Python example uses TCP there.

Defaults are shown above except `max_workers`, which defaults to 1. `concurrency` describes how many simultaneous requests one worker can handle. When all ready workers remain saturated for `scale_delay`, ooth starts another, up to `max_workers`. This maintains spare capacity using worker events; it does not inspect the kernel's pending-connection count. Workers with no active requests for `idle_timeout` are stopped down to `min_workers`, including zero. A required dependency retains at least one worker. `request_timeout: 0s` disables the request watchdog.

Configuration is validated as a whole before applying it. Invalid YAML, missing dependencies, and cycles retain the previous running configuration. New listeners are bound before changing existing services; a bind failure is retried. Updating a service gracefully retires its old workers and reuses an unchanged listener. Retiring workers count against `max_workers`, so an update can queue requests while they drain. Removing an app closes its parent listener and stops its workers. A removed or malformed app file that leaves unresolved dependencies causes the entire snapshot to be rejected.

## Virtual activation and dependencies

An app without `listen` is a virtual activation target. It needs no inherited socket or ooth protocol:

```yaml
name: database
command: [/usr/bin/my-database, --foreground]
ready: tcp://127.0.0.1:5432
```

The first demand for `web` activates `database` and `cache` together. `web` starts after both are ready. Shared prerequisites start once. This is an internal dependency graph, not an additional control socket.

- `ready: started`: ready as soon as process creation succeeds; default for virtual services. This orders process creation, not application initialization.
- `ready: event`: wait for the stdout `ready` event; default for socket workers.
- `ready: tcp://host:port` or `unix://path`: connect to the endpoint until it responds. Probes establish and close a real connection, so use an endpoint that tolerates that.
- `startup: true`: activate at supervisor startup and keep at least one worker.

Virtual services have `max_workers: 1`. They remain running while needed, then stop after their idle timeout. Losing a prerequisite stops dependent workers; demand reactivates the dependency graph. Supervisor shutdown drains dependents before prerequisites. The scheduler does not require unrelated branches to wait for one another.

## Worker convention

1. Convert inherited stdin into a listening socket: fd 0 on Unix, `STD_INPUT_HANDLE` on Windows. Do not read request bytes from stdin as a stream.
2. Use stdout exclusively for events when `ready: event`; send logs to stderr.
3. Emit `ready`, then `start` and `end` for every request. Flush each complete line. Concurrent workers must serialize writes to their own event stream.
4. On the graceful signal, stop accepting new requests, finish active requests, then exit.

```text
v=1 event=ready ts=1790193600000000000
v=1 event=start ts=1790193600001000000 id=42
v=1 event=end ts=1790193600002000000 id=42 duration_ns=1000000
```

No JSON, escaping, or control channel. Fields are space-separated `key=value` pairs; values are printable ASCII without spaces or `=`. Request IDs are unique among active requests within one worker. `ts` is Unix time in nanoseconds; `duration_ns` is elapsed request time in nanoseconds, preferably measured with a monotonic clock. Supervisor deadlines use its own monotonic clock, so worker clock changes cannot extend them. Unknown/duplicate fields, oversized lines (4096-byte limit), invalid event order, and a broken event stream stop that worker and enter the restart policy.

The environment contains `OOTH_WORKER=1` and `OOTH_CONCURRENCY`. Configuration cannot override `OOTH_*`. No ooth library is needed in the worker. See [examples/worker.py](examples/worker.py). Existing FastCGI programs may inherit stdin in a compatible way but need telemetry integration to support request-based scaling and idle detection. With `ready: started`, ooth cannot infer their active requests or perform telemetry-based scaling/shrinking.

## Platform details

Both platforms use `syscall.EpollCreate1`, `EpollCtl` and `EpollWait`. One goroutine waits for **all listeners**; there is no goroutine or periodic readiness check per listener. ooth registers `EPOLLIN | EPOLLONESHOT`, then rearms delivered listeners through the supervisor's existing 25 ms tick. This bounds notifications while a connection waits for a starting worker. Unique registration IDs discard events from removed configurations. A private loopback UDP socket wakes the poller during shutdown; it is not a worker control channel.

Linux uses the existing kernel epoll implementation. The old `Read(nil)` change is gone; the only syscall addition on Linux is `EpollClose(int)`, a portable close spelling shared with the Windows extension. Shutdown sends `SIGTERM`, then kills the worker's cgroup after `stop_timeout`, or uses `Process.Kill` (`SIGKILL`) when cgroups are unavailable.

Windows needs more than that Unix patch. A connected socket's zero-byte `WSARecv` already waits for data, but a listening socket does not provide that operation. [Issue 15735](https://github.com/golang/go/issues/15735) and the tests in [CL 22031](https://go-review.googlesource.com/c/go/+/22031) concern connections after `Accept`; the comment quoted in [issue 27315](https://github.com/golang/go/issues/27315) points to that proposal. Local tests on stock Go 1.24.3 and 1.27.1 reproduced this distinction.

The Windows toolchain patch:

- Adds the epoll socket API using asynchronous `IOCTL_AFD_POLL` requests and one IOCP per poller. A separate AFD device handle owns the IOCP association; monitored sockets are not attached to it. Requests remain pinned until their completion packets are drained, including cancellation and close. No Rust, C, CGO, libuv or wepoll binary dependency is needed.
- Uses the standard `exec.Cmd.Stdin` inheritance path. The former `StartProcess` override was removed after TCP and Unix lifecycle tests passed without it. Listener initialization first tries normal IOCP. Only when this fails with `ERROR_INVALID_PARAMETER` on a listening socket does it enable Go's existing local-event I/O path, adding deadline/close tracking. This follows libuv's imported-socket fallback approach. The inherited `AcceptEx` fallback checks deadline/close every 20 ms; that worker-side wait is separate from the supervisor's event-driven AFD poller. Connected sockets retain Go's normal I/O implementation.
- Adds the portable `exec.Cmd.NewProcessGroup` option (a no-op on Linux) and supports `Process.Signal(os.Interrupt)`. Visible GUI windows receive `WM_CLOSE`; console groups receive `CTRL_BREAK_EVENT`. A supervisor started without a console allocates a hidden console for its console workers.
- Adds `syscall.SysProcAttr.JobObjects` and passes those handles through `PROC_THREAD_ATTRIBUTE_JOB_LIST` during process creation, before child code can run. Requires Windows 10 / Server 2016 or newer. Job creation, queries and termination live in `job_windows.go`, outside the toolchain patch. No `taskkill`, PowerShell, or shell command is launched by ooth.

Workers must handle the graceful notification. A GUI may reject `WM_CLOSE`; a fully detached headless process has no universal graceful Windows notification. Such a process reaches the forceful timeout. Windows SCM service control is not implemented. Graceful shutdown still asks the foreground worker to drain its work; forced shutdown terminates its whole Job, including descendants.

### Descendant ownership

The common application API is deliberately small: `processOwner.Start/Wait/Close` and `processJob.Kill/Close/Members`. `Wait` uses `exec.Cmd.Wait` to observe direct-child exits, including crashes, without polling. Before the supervisor removes or restarts a worker, it terminates any remaining descendants and drains cleanup. A worker exiting ends that worker instance: daemonizing children does not keep the service alive. Final stdout events are drained, with a 100 ms deadline for a pipe retained by other processes.

Windows creates one private, non-inheritable Job handle per worker instance, enables `KILL_ON_JOB_CLOSE`, and permits no breakaway. The process is assigned atomically at creation; descendants inherit membership automatically. `TerminateJobObject` terminates the family. Closing ooth also closes its Job handles and terminates their members. Cleanup waits on remaining process handles, checking Job membership to avoid PID reuse mistakes. See [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects) and [creation-time Job assignment](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute).

Linux creates a cgroup per worker and uses Go's existing `UseCgroupFD/CgroupFD` fields for atomic placement with `clone3`. `cgroup.kill` covers descendants, including nested cgroups; cleanup waits for `cgroup.events` to become empty using epoll. No process-tree scan reconstructs ancestry. The subreaper uses `prctl` and `SIGCHLD`; it examines current adopted children only when notified, excluding managed workers so it cannot steal their `Cmd.Wait` result. The previous once-per-second PID 1 scan is gone. These utilities require no new Linux toolchain patch.

Full Linux ownership needs cgroup v2, `clone3` with `CLONE_INTO_CGROUP` (Linux 5.7+) and `cgroup.kill` (Linux 5.14+), with permission to create subgroups and place processes there. **Root is not required** when a subtree is delegated to the user, for example by a service manager using `Delegate=yes`. Migration permissions also matter when ooth starts outside that subtree. Containers may expose read-only cgroups or block `clone3`. If setup is unavailable, ooth warns and continues with direct children; if placement fails but the command succeeds without placement, it warns and disables placement for subsequent workers. PID 1 zombie reaping remains active. See the kernel's [cgroup v2 documentation](https://docs.kernel.org/admin-guide/cgroup-v2.html).

Kernel membership is authoritative; there is no promised stream of every descendant birth/exit. These groups supervise cooperative services, not malicious workers deliberately migrating themselves or launching through unrelated system services. Linux cgroups persist if ooth itself is forcibly killed: automatic recovery of those groups is not implemented. On normal shutdown or a worker crash, ooth cleans the groups before proceeding.

Go workers that share a Windows listener must use this patched toolchain too, including when converting stdin with `net.FileListener`. The [handoff probe](tools/probes/README.md#windows-listener-handoff) refines this requirement: with stock Go 1.27.1 the first worker converts stdin and replies, but the next worker fails during `net.FileListener`, both with concurrent workers and after killing/waiting for the first. The handle is valid; adopting the shared listener into another IOCP fails. Three workers pass with the patched compiler.

Other languages need their native inherited-socket support. PHP's TCP FastCGI accept path retrieves the native handle with `_get_osfhandle` and calls `accept`; that retrieval does not detach an IOCP. libuv already handles failed IOCP association for imported sockets using local events. Local Node.js 24.19.0 probes made three workers accept using its private native-handle binding, but public `listen({fd: 0})` and `listen(process.stdin)` failed on Windows. Node documents that listening on a file descriptor is unsupported there. The private-binding probe is diagnostic, not a supported Node worker adapter; PHP was inspected in source, not tested as an executable. See the probe notes for sources and reproduction steps.

A further [Node 26.10.0 probe](tools/probes/README.md#node-through-stdin-alone) recovers the socket directly from stdin using built-in `node:ffi` and either `GetStdHandle` or `_get_osfhandle(0)`. Three concurrent workers passed without an additional inherited handle, handle-number environment variable or patched parent. Node still needs its private TCPWrap binding to adopt the Windows socket. The experimental fixture `tools/probes/node_worker.cjs` implements ooth telemetry and graceful draining; set `OOTH_TEST_NODE` to the Node executable to enable its lifecycle test. On Linux the fixture uses the public fd API. No Node/npm dependency was added to ooth, and the Windows adapter is not a stable public API.

### Reusable epoll extension

The implementation is in [`tools/patchgo/patches/epoll_windows.txt`](tools/patchgo/patches/epoll_windows.txt), installed as `syscall/ooth_epoll_windows.go`. Its API has no knowledge of workers, configuration or ooth. The AFD mechanism follows the approach demonstrated by [wepoll](https://github.com/piscisaureus/wepoll) and [libuv](https://github.com/libuv/libuv/blob/v1.x/src/win/poll.c); attribution is included in `THIRD_PARTY_NOTICES.md`.

- Supports `EpollCreate`, `EpollCreate1`, `EpollCtl`, `EpollWait` and `EpollClose` on Windows amd64/arm64, for native Winsock sockets including TCP, UDP and Unix sockets.
- Supports ADD/MOD/DEL, level triggering and `EPOLLONESHOT`; events include IN, OUT, PRI, ERR, HUP and RDHUP. Unsupported flags, including `EPOLLET`, are rejected. This is a documented subset, not complete Linux epoll compatibility.
- Each underlying socket has one owning AFD poller. DEL before closing or reusing its handle, including duplicated/inherited handles. One caller performs Wait; control operations may run concurrently. Fd/Pad in `EpollEvent` are opaque application data, not the socket handle; pass the full-width handle as the `EpollCtl` fd argument.
- Use `EpollClose`, not Windows `CloseHandle`, to cancel and drain pending requests safely. AFD structures use an internal Windows interface. The backend is experimental and tested on the supported build matrix; other providers and Windows versions need validation.

`epoll_test.go` exercises readiness without consuming traffic, one-shot rearming, level triggering, pending MOD/DEL/re-add, shutdown cancellation, connection writability/half-close and 128 idle listeners without per-socket goroutine growth. Run `go test -run '^$' -bench BenchmarkEpollIdleListeners -benchmem .` with the dedicated compiler for an idle-queue microbenchmark; it is not an application-throughput comparison.

## Development status

The supervisor is in `main.go`; platform process utilities are in `job_linux.go` and `job_windows.go`, and toolchain changes in `tools/patchgo`. Integration tests cover descendant ownership across immediate parent exit, forced tree cleanup, crash restart, Windows breakaway rejection, atomic assignment failure and the Linux warning/fallback path. Set `OOTH_CGROUP_ROOT` to a delegated subtree and run Linux tests from within that delegation to enable the cgroup cases. The earlier prototype under `src/` and original patch under `golang_patch/` are retained as historical material, excluded from the root module's builds.

Integration tests cover cold activation, TCP/Unix listeners, multiple worker processes, idle return to zero, virtual prerequisites, added/reloaded/removed applications, rejected configuration, graceful draining and forced termination. Set `OOTH_TEST_PYTHON` to an interpreter's absolute path to test the Python example as well. The workflow enables that test. The Linux suite also passed `go test -race` locally. Additional [platform probes](tools/probes/README.md) verified actual Linux PID 1 orphan reaping, Windows GUI shutdown and operation without a console or inherited standard handles. These are bounded checks, not a claim of compatibility with every worker/runtime.

Dependencies: `sgtdi/fswatcher v1.3.0`, `goccy/go-yaml v1.19.2`, and fswatcher's indirect `golang.org/x/sys`. [Canonical Pebble](https://github.com/canonical/pebble) is an architectural reference, not a dependency.
