# ooth

Minimal process supervisor with lazy socket activation, written in Go. The application lives in **one `main.go`**. Linux and Windows builds use `CGO_ENABLED=0`; the executable does not need Go, a shell, or external management commands at runtime. Windows still uses the operating system's DLLs.

ooth owns TCP or Unix listening sockets. Incoming connections wake a worker pool; each worker inherits the listener on standard input and accepts connections itself. **ooth never accepts, reads, copies, or proxies application traffic.** “Copyless” here means no forwarding through the supervisor, not that the operating system or application performs no copies.

This is an experimental nucleus, not a replacement for all of systemd. It supervises foreground processes, orders dependencies, restarts failed workers with backoff, grows busy pools, and removes idle workers. As Linux PID 1 it also reaps adopted zombie processes through `/proc`; it does not mount filesystems, configure the machine, or implement a service manager for Windows SCM.

## Build and run

```sh
bash ./build.sh
./bin/ooth-linux-amd64 -check -config examples/ooth.yaml
./bin/ooth-linux-amd64 -config examples/ooth.yaml
curl http://127.0.0.1:8080/
```

On Windows use Git Bash to build, then run `bin/ooth-windows-amd64.exe`. Set the example's `command` to the absolute path of your Python interpreter if `python3` is not available. Production workers may use any language; Python is only an example.

The script downloads and verifies **Go 1.27.1**, patches its dedicated copy under `build/`, runs tests and `go vet`, then builds amd64 and arm64 binaries for the host OS. It never patches an existing Go installation. The Go version is deliberately pinned: these small internal changes must be reviewed when upgrading Go. A stock `go build` is not supported.

`-version` prints the commit/build version; `-debug` includes completed request metrics. The GitHub workflow tests on Linux and Windows on push. Run the workflow manually to publish a prerelease containing the four binaries and `SHA256SUMS`; arm64 is cross-compiled, while integration tests run on amd64.

## Configuration

Main `ooth.yaml`:

```yaml
watch:
  - /var/www/app*/ooth.yaml
```

Windows paths may use forward slashes, for example `C:/www/app*/ooth.yaml`. Patterns follow Go's `filepath.Glob`: `*` matches within one directory level; recursive `**` is not supported. Relative paths resolve against the YAML file that contains them. Changes and new matching files are watched with [sgtdi/fswatcher](https://github.com/sgtdi/fswatcher); a five-second rescan also reconciles missed events. YAML uses [goccy/go-yaml](https://github.com/goccy/go-yaml), with unknown fields rejected.

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

Linux uses the original zero-length-read idea: the dedicated Go patch makes `File.Read(nil)` wait through Go's existing network poller without resetting the pending readiness edge. ooth does not implement its own epoll loop. It sends `SIGTERM`, then `Process.Kill` (`SIGKILL`) after `stop_timeout`.

Windows needs more than that Unix patch. A connected socket's zero-byte `WSARecv` already waits for data, but a listening socket does not provide that operation. [Issue 15735](https://github.com/golang/go/issues/15735) and the tests in [CL 22031](https://go-review.googlesource.com/c/go/+/22031) concern connections after `Accept`; the comment quoted in [issue 27315](https://github.com/golang/go/issues/27315) points to that proposal. Local tests on stock Go 1.24.3 and 1.27.1 reproduced this distinction.

The Windows toolchain patch:

- Waits for listener readiness with `WSAPoll`, without accepting anything. It checks deadline/close state every 20 ms; this uses an OS wait thread per active wait and is a known scaling cost.
- Inherits the Winsock handle directly. Shared listeners use local completion events instead of binding the shared socket to one process's IOCP. Connected sockets retain Go's normal I/O implementation.
- Adds the portable `exec.Cmd.NewProcessGroup` option (a no-op on Linux) and supports `Process.Signal(os.Interrupt)`. Visible GUI windows receive `WM_CLOSE`; console groups receive `CTRL_BREAK_EVENT`. A supervisor started without a console allocates a hidden console for its console workers.
- Uses `Process.Kill` after the configured timeout. No `taskkill`, PowerShell, or shell command is launched by ooth.

Workers must handle the graceful notification. A GUI may reject `WM_CLOSE`; a fully detached headless process has no universal graceful Windows notification. Such a process reaches the forceful timeout. Windows SCM service control is not implemented. Process termination targets the direct worker; descendants must be managed by the worker rather than daemonized independently.

Go workers that share a Windows listener must use this patched toolchain too, including when converting stdin with `net.FileListener`. Other languages need their native inherited-socket support. This compatibility requirement is why the Windows patch is more substantial than the Linux patch.

## Development status

The application is in `main.go`; integration helpers are in `main_test.go`, and toolchain changes in `tools/patchgo`. The earlier prototype under `src/` and original patch under `golang_patch/` are retained as historical material, excluded from the root module's builds.

Integration tests cover cold activation, TCP/Unix listeners, multiple worker processes, idle return to zero, virtual prerequisites, added/reloaded/removed applications, rejected configuration, graceful draining and forced termination. Set `OOTH_TEST_PYTHON` to an interpreter's absolute path to test the Python example as well. The workflow enables that test. The Linux suite also passed `go test -race` locally. Additional [platform probes](tools/probes/README.md) verified actual Linux PID 1 orphan reaping, Windows GUI shutdown and operation without a console or inherited standard handles. These are bounded checks, not a claim of compatibility with every worker/runtime.

Dependencies: `sgtdi/fswatcher v1.3.0`, `goccy/go-yaml v1.19.2`, and fswatcher's indirect `golang.org/x/sys`. [Canonical Pebble](https://github.com/canonical/pebble) is an architectural reference, not a dependency.
