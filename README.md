# ooth

Minimal process supervisor with lazy socket activation, written in Go. The supervisor lives in **`main.go`**, with process ownership utilities in `job_linux.go` and `job_windows.go` and optional Windows service integration in `scm_windows.go`, selected by Go at compilation. Linux and Windows builds use `CGO_ENABLED=0`; the executable does not need Go, a shell, or external management commands at runtime. Windows still uses the operating system's DLLs.

ooth owns TCP or Unix listening sockets. Incoming connections wake a worker pool; each worker inherits an additional listener handle, reads its number from `OOTH_LISTEN_HANDLE`, and accepts connections itself. Legacy stdin handoff is configurable. **ooth never accepts, reads, copies, or proxies application traffic.** “Copyless” here means no forwarding through the supervisor, not that the operating system or application performs no copies.

An app without `listen` runs as an ordinary process pool. The same minimum, maximum, telemetry, scaling and shutdown rules apply; the program can open its own listener. There is no special reuseport mode in ooth.

This is an experimental nucleus, not a replacement for all of systemd. It supervises foreground processes, orders dependencies, restarts failed workers with backoff, grows busy pools, and removes idle workers. Windows Jobs and Linux cgroup v2 keep descendants associated with their worker even when intermediate parents exit. Linux reaps adopted orphans both as PID 1 and, using subreaper mode, as an ordinary process. This is independent of cgroup availability. Optional Windows SCM integration lets ooth run as a service and control registered services. It does not mount filesystems or configure the machine.

## Build and run

The [worker lifecycle matrix](tools/probes/runtime/README.md) uses real ooth activation, request telemetry, growth, idle zero, reactivation and graceful draining, directly and through Caddy/Nginx. Node, Bun and Deno serve HTTP/1.1 and h2c; Python and Socketify serve HTTP/1.1. WebSocket upgrade/echo/close tests cover the JavaScript workers and Socketify. These are bounded integration tests, not performance certification.

The [Socketify ctypes adapter](tools/probes/socketify/README.md) uses the actual native HTTP/1.1 and WebSocket server. Its protocol mode supplies request events and stdin stop, keeping handlers alive after closing the listener. It reinitializes an internal uSockets poll after closing a temporary listener, requires pinned native binaries, and needs no C/C++ compilation.

The [JavaScript adapters](tools/probes/javascript/README.md) use a private binding for Node on Windows, Bun's fd-capable TCP server on Linux and FFI accept thread on Windows, and FFI Winsock accept for Deno Windows. They feed the runtimes' HTTP compatibility servers; Deno's direct Windows listener adoption still fails. The bounded WebSocket fixture uses the HTTP upgrade event. These runtime adapters remain experimental.

[TrueAsync Windows](tools/probes/trueasync/README.md) also has an ooth protocol worker using its built-in HTTP/1.1, h2c and WebSocket server through a private FFI hook. The released Linux binary is static and lacks FFI, so native HTTP-server adoption remains blocked there; only async accept is verified. The user chose no runtime recompilation. Stock PHP-CGI remains FastCGI without telemetry, with its existing Windows launcher.

The separate [worker-owned listener suite](tools/probes/reuseport/README.md) tests ordinary process pools whose workers bind their own TCP sockets on Linux. It includes TrueAsync's native server without FFI. Its tests, results and generated configurations are distinct from the inherited-socket matrix above.

```sh
bash ./build.sh
./bin/ooth-linux-amd64 -check -config examples/ooth.yaml
./bin/ooth-linux-amd64 -config examples/ooth.yaml
curl http://127.0.0.1:8080/
```

On Windows use Git Bash to build, then run `bin/ooth-windows-amd64.exe`. Set the example's `command` to the absolute path of your Python interpreter if `python3` is not available. Production workers may use any language; Python is only an example.

The script downloads and verifies **Go 1.27.1**, patches its dedicated copy under `build/`, runs tests and `go vet`, then builds amd64 and arm64 binaries for the host OS. It never patches an existing Go installation. When the patch changes, affected source files are restored from the verified archive before applying it. The Go version is deliberately pinned: toolchain changes must be reviewed when upgrading Go. A stock `go build` is not supported.

`-version` prints the commit/build version; `-debug` includes request start/finish metrics and listener observations with their poll-return timestamps. Development changes are tested locally; pushes do not trigger GitHub builds. Run the workflow manually at agreed milestones to verify Linux and Windows on clean runners. Enable `publish_release` when that run should also publish a prerelease containing the four binaries and `SHA256SUMS`; otherwise it only builds/tests and uploads workflow artifacts. ARM64 is cross-compiled, while integration tests run on amd64.

## Configuration

Main `ooth.yaml`:

```yaml
watch:
  - /var/www/app*/ooth.yaml
resources:
  max_cpu_percent: 90
  min_available_memory_percent: 10
```

Windows paths may use forward slashes, for example `C:/www/app*/ooth.yaml`. `*` matches within one directory level; a complete `**` path component matches zero or more levels. For example, `/var/www/**/ooth.yaml` finds both `/var/www/ooth.yaml` and `/var/www/team/site/ooth.yaml`. `?` and character classes retain Go's `filepath.Match` syntax. Recursive discovery does not follow directory symlinks. Relative paths resolve against the YAML file that contains them. Changes and new matching files are watched with [sgtdi/fswatcher](https://github.com/sgtdi/fswatcher); a five-second rescan also reconciles missed events.

YAML uses [goccy/go-yaml](https://github.com/goccy/go-yaml). Both the main configuration and application files ignore unknown fields, including inside their structured settings, so a file may also contain settings for other tools. Known ooth fields still require valid types and values; malformed YAML, duplicate keys and multiple documents are rejected.

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
socket_handoff: env
requires: [database, cache]
min_workers: 0
max_workers: 4
concurrency: 1
idle_timeout: 1m
start_timeout: 10s
stop_timeout: 10s
request_timeout: 0s
scale_at: 80%
scale_window: 1s
```

`command` is an argument list, executed directly without a shell. Workers must remain in the foreground. `name` defaults to the containing directory's name. Networks are `tcp`, `tcp4`, `tcp6`, and `unix`. On Windows, the worker's own language/runtime must also support Unix sockets; CPython's Windows build does not currently implement their accept path, so the Python example uses TCP there.

`socket_handoff` selects how the listener reaches a worker:

| Value | Behavior |
| --- | --- |
| `env` or omitted | Default on both OSes: extra inherited listener; `OOTH_LISTEN_HANDLE` contains its decimal fd/native handle. Linux currently assigns fd 3. stdin remains a pipe. |
| `stdin` or `0` | Listener replaces stdin, for stock PHP-CGI and other compatible programs. No stdin stop command is possible. |
| `3` through `1024` | Linux only: assign that exact fd, also publish its value in `OOTH_LISTEN_HANDLE`, and retain the stdin pipe. |

Windows does not map additional native handles to arbitrary CRT fd numbers, so numeric choices other than `0` are rejected there. stdout (`1`) and stderr (`2`) are reserved for output and logs. The environment variable communicates an **already inherited socket**, not an address or a handle that can be opened independently. This changes the earlier stdin default; existing stdin-based workers must set `socket_handoff: stdin` explicitly.

Defaults are shown above except `max_workers`, which defaults to 1. The six pool controls are `min_workers`, `max_workers`, `concurrency`, `scale_at`, `scale_window` and `idle_timeout`. `concurrency` is the sustainable simultaneous request count declared per worker; durations alone cannot reveal it. `scale_at` accepts a number or percentage, for example `80` or `80%`.

ooth integrates occupancy at every start/end and evaluates successive complete `scale_window` intervals. Each ready telemetry worker contributes at most `concurrency` busy slots. Two workers with concurrency four averaging six active slots have 75% occupancy. An average at or above `scale_at` requests one extra worker, up to `max_workers`. A changed pool or any unready/non-telemetry member resets the window: new capacity must complete a fresh window before another increase. Brief requests between scheduler ticks count. These are consecutive windows, not a sliding average. The legacy `scale_delay` spelling aliases `scale_window` with this new behavior; specifying both is rejected.

Global `resources` limits gate extra growth. One background sampler uses [gopsutil/v4](https://github.com/shirou/gopsutil) approximately once per second for CPU utilization and available RAM, without CGO or external commands on Linux/Windows. Defaults defer growth at 90% CPU or below 10% available RAM. Zero disables each threshold. Before the first sample, on measurement errors, or after three seconds without a fresh sample, extras wait. Errors warn on stderr; logs show pause/resume and debug samples. Limits reload without restarting workers. Cold activation, configured minimum, startup services and dependencies still start; the guard never stops running workers and is not a hard resource reservation.

Linux also reads visible cgroup v2 ancestors of ooth and its worker delegation: CPU quota/cpuset capacity against usage deltas, and `memory.max` remaining after `memory.current`. Sibling consumption counts. The highest CPU utilization and lowest available-memory percentage across these scopes and the host govern growth. Cgroup memory accounting includes charged cache, making this reserve conservative. Reads need no writable delegation. Hidden ancestors, cgroup v1 and Windows container/Job quotas are not normalized; Windows uses host metrics.

Listener notifications activate empty pools; they do not measure universal request pressure. Workers without the stdout handshake have no request-based scaling. Idle telemetry workers stop after `idle_timeout`, down to `min_workers`, including zero; required dependencies retain at least one. `request_timeout: 0s` disables the request watchdog.

Configuration is validated as a whole before applying it. Invalid YAML, missing dependencies, and cycles retain the previous running configuration. New listeners are bound before changing existing services; a bind failure is retried. Updating a service gracefully retires its old workers and reuses an unchanged listener. Retiring workers no longer count toward active capacity or `max_workers`: replacements may start while they drain. Removing an app closes its parent listener and stops its workers. A removed or malformed app file that leaves unresolved dependencies causes the entire snapshot to be rejected.

### Arguments and environment placeholders

Every element of `command` and every value in `env` supports Go `text/template`
expressions. This applies to socket workers and ordinary processes, on every
spawn and restart. Each expanded argument stays one argument: spaces are
preserved, `$NAME` is literal, and no shell or recursive expansion is involved.
Unknown dot-notation fields or malformed templates reject the configuration.
Expansion errors name the affected field without printing its value.

| Placeholder | Value |
| --- | --- |
| `{{.name}}` | Configured application name. |
| `{{.directory}}`, `{{.source}}` | Resolved working directory and application YAML path. |
| `{{.env.NAME}}` | Environment inherited by ooth; this does not reference the app's `env` map. |
| `{{.vars.NAME}}` | Literal string from the app's optional `vars` map, reusable across arguments and environment. |
| `{{.listen.network}}`, `{{.listen.address}}`, `{{.listen.url}}` | Inherited listener metadata, available only with `listen`. URLs use `tcp://` or `unix://` and escape paths. |
| `{{.listen.host}}`, `{{.listen.port}}` | TCP listener host and port, including the actual assigned port when binding port zero. |
| `{{.listen.path}}` | Resolved Unix socket path. |
| `{{.config.custom.key}}` | The original application YAML, including extra fields belonging to other tools. Use `index .config "key-with-dashes"` for such keys. |
| `{{.runtime.pid}}` | The instance PID; zero before spawn and for app-scoped actions. |
| `{{.runtime.workers}}`, `{{.runtime.ready_workers}}`, `{{.runtime.stopping_workers}}`, `{{.runtime.pids}}` | Snapshot of the current app lifecycle generation; workers includes reserved starts and excludes retirees/exited instances. |
| `{{.runtime.requests}}`, `{{.runtime.worker_requests}}` | In-flight requests reported by the generation and by this instance. |
| `{{.runtime.ready}}`, `{{.runtime.telemetry}}`, `{{.runtime.stopping}}` | Instance state; false where no instance exists. |
| `{{.runtime.started_ns}}`, `{{.runtime.uptime_ms}}` | Startup timestamp and elapsed startup time; zero before spawning or for app scope. |
| `{{.runtime.exit_code}}` | Direct process exit code in `post_stop`, otherwise -1; SCM does not expose a POSIX-style exit code here. |
| `{{.runtime.event}}`, `{{.runtime.scope}}` | `launch` or the action trigger name; `worker` or `app`. |

The same evaluator expands `scm.args` and every string parameter of an action:
arguments, environment keys/values, action directory, HTTP URL/method/headers/body,
Unix transport path, TCP/UDP address/payload and expected text/regexp. Timing,
numeric expectations and trigger policy remain typed YAML fields. `vars`, the
app-level `directory`, `ready`, and `listen` remain literal configuration.
`.config` is the source document, not an expanded copy and not a map of defaults.
Changing an extra field now reloads the app too, since a template may use it.
Validation uses the configured address and zero-valued runtime data; execution
uses the bound listener's actual address and a fresh immutable state snapshot.
One invocation's retries share that snapshot; the next periodic invocation gets
a new one. Runtime values are unavailable before their event, not predictions
of the future PID or pool. A listener handle is process-local and is not exposed
to actions as an inheritable capability. Quote expressions in YAML.
For example, an argument `"--endpoint={{.vars.endpoint}}"` and environment value
`ENDPOINT: "{{.vars.endpoint}}"` receive the same string.

### Actions, lifecycle hooks and health checks

Define named `actions` inside an **application YAML**, then attach them separately
under `triggers`. Definitions describe execution and matching; a binding chooses
the event, `scope: worker|app`, whether to `wait`, and failure policy. No shell,
CGO, extra dependency or Go patch is involved. CEL is deferred.

```yaml
name: api
command: [my-server]
min_workers: 1
start_timeout: 45s
vars:
  health_url: http://127.0.0.1:8080/health
actions:
  healthy:
    http:
      url: '{{.vars.health_url}}'
      method: GET
      headers: {X-Probe: '{{.name}}'}
    expect:
      status: [200]
      contains: ready
      regexp: 'ready|healthy'
    timeout: 2s
    retries: 2
    backoff: 100ms
  prepare:
    command: [my-tool, prepare, '--app={{.name}}']
    env: {APP_SOURCE: '{{.source}}'}
    expect: {exit_codes: [0], contains: prepared}
  report:
    command: [my-tool, record, '{{.runtime.event}}', '{{.runtime.pid}}']
triggers:
  pre_start:
    - {action: prepare, scope: app, wait: true}
  post_start:
    - {action: report, scope: worker, wait: false}
  readiness:
    - {action: healthy, scope: app, interval: 1s, failure_threshold: 5}
  health:
    - {action: healthy, scope: app, interval: 10s, failure_threshold: 3, on_failure: restart}
  pre_stop:
    - {action: report, scope: worker, wait: false}
  post_stop:
    - {action: report, scope: app, wait: true}
```

Exactly one action kind is required:

| Kind | Parameters and success |
| --- | --- |
| `command: [executable, args...]` | Direct execution in the app's account/directory/environment. Optional action `directory` (relative to app directory) and `env` override those values. Exit zero by default, or one of `expect.exit_codes`; text matching inspects stdout. stderr is discarded and stdout never enters the worker telemetry parser. |
| `http: {url, method, headers, body, unix_socket}` | GET by default; optional `unix_socket` connects HTTP through that Unix path instead of TCP. Status 2xx by default, or one of `expect.status`; text matching inspects the response body. Redirects are not followed. |
| `tcp: {address, send}` | Connect to host:port. Optional `send` writes a payload. Without a text expectation the connection/write is sufficient; otherwise read until matching or timeout/EOF. |
| `udp: {address, send}` | Send a datagram and validate the first reply. Both payload and a text expectation are required: a successful UDP send alone does not verify the peer. |

`expect.contains` and `expect.regexp` are optional and, when both are present,
both must match. Regexps use Go syntax. Network and execution errors fail the
attempt before text matching. Output is bounded at 1 MiB; excess output fails.
TCP matching may succeed on a received prefix without waiting for EOF.

An action defaults to `timeout: 3s`, `retries: 0` and `backoff: 100ms`. Retries
are **additional attempts**, each with its own timeout; delays double up to 30s.
Command timeout/cancellation kills its owned Job/cgroup, with the usual Linux
direct-child fallback when a cgroup is unavailable. The command is a finite
action, not a way to launch an unsupervised daemon. Captured output and expanded
parameters are not written to logs; logs identify action, event and failure.

Bindings default to `scope: worker` and `wait: true`. Bindings within an event
execute concurrently. Awaited bindings gate that phase; `wait: false` is a
bounded background notification whose outcome only logs, without changing
readiness or restarting anything. `on_failure: restart` is the default for
awaited start/readiness/health bindings; `log` is the default for stop hooks
and unawaited bindings. `restart` is rejected for stop hooks or `wait: false`.

| Trigger | Meaning |
| --- | --- |
| `pre_start` | Before OS spawn. An awaited failure prevents spawn and applies restart backoff; `on_failure: log` permits it. Reserved starts count toward capacity. |
| `post_start` | After spawn (SCM: after Running). Awaited hooks must complete before readiness; failure normally retires the instance. |
| `readiness` | After the existing `ready` criterion and awaited post-start hooks. Repeats until first success; all awaited checks must pass. |
| `health` | Starts after initial readiness checks pass, repeats at `interval`, with no overlapping invocation of the same binding. |
| `pre_stop` | Before sending stdin/OS/SCM graceful stop. Awaited hooks consume the existing `stop_timeout`, never extend it. Failure logs and proceeds; deadline forces termination. |
| `post_stop` | After process exit and family cleanup, also after a crash. Awaited hooks complete before shutdown considers that instance fully retired. |

Readiness and health default to `interval: 1s` and `failure_threshold: 3`;
the interval starts when the preceding invocation finishes. A failure means an
entire invocation exhausted its action retries. Success resets consecutive
failures. Readiness stays false until success and is still bounded by
`start_timeout`. Health only marks the instance/app unready at the threshold;
`restart` retires it, while `log` leaves it running and a subsequent success
restores readiness. Unawaited checks do not affect these states. Startup and
health actions are cancelled on retirement. Stop hooks have their own bounded
action timeout; a pre-stop action is cancelled if its worker exits. ooth waits
for outstanding action cleanup on shutdown, including background notifications.
HTTP and socket checks send real traffic, which may reset idle timers or create
telemetry load. Choose the interval accordingly for scale-to-zero applications.

App scope runs once for an active generation, rather than once for each scaled
worker: pre-start before its first spawn, post-start after its first spawn,
pre-stop when its last active member is retired, and post-stop after its last
member exits. Readiness/health results apply to all members of that generation;
an app-scoped failure restarts all of them. Once every member is retiring, new
demand/minimum may create a new generation while the old one drains. Thus old
post-stop actions can overlap new pre-start actions; they use their original
configuration and cannot change the new generation's state. Hooks must account
for shared external resources. Scaling by itself does not repeat app hooks.

A check against a shared listener proves that **some** worker answers, not which
PID answered. Use app scope for that probe, or a worker-specific endpoint/command
using `.runtime.pid` when individual readiness matters. A TCP connect to ooth's
parent listener alone only proves the listening socket exists. Readiness gates
supervision and dependencies; ooth does not intercept traffic or prevent an
already accepting worker from receiving requests. Waiting pre-stop hooks delay
the graceful request; use `wait: false` for a notification that must not delay it.

### Dependency conditions

`requires: [database]` retains its existing meaning: activate database and wait
for readiness before starting this app. Alternatively specify conditions:

```yaml
dependencies:
  database: ready
  logger: started
  cache: parallel
```

`started` waits for an existing non-retiring process, without its readiness;
`ready` waits for full readiness including actions. `parallel` activates both
apps concurrently but this app remains unready until that dependency is ready.
All kinds hold the dependency alive, participate in cycle detection and reverse
shutdown order. A name cannot occur in both `requires` and `dependencies`.
Loss of a `started`/`ready` prerequisite retires dependent workers as before;
loss of a `parallel` prerequisite removes readiness without stopping them.
Configured minimum workers may start together even while readiness is pending.

This separation draws on [Pebble health checks](https://ubuntu.com/docs/pebble/reference/health-checks/)
while using app-local actions and explicit lifecycle/dependency readiness gates.
The runnable [actions example](examples/actions/ooth.yaml) uses the existing
Python worker on port 8081: run `ooth -config examples/actions-root.yaml`.
`actions_test.go` preserves native Linux/Windows execution, protocol matching,
retries, scope, dependency, cancellation, stop-deadline and recovery tests.

### Restart an app when files change

Each application can select filesystem changes that retire its current processes:

```yaml
restart_on:
  - glob: config/*.yaml
    events: [create, write, remove, rename]
  - glob: src/**/*.js
  - glob: public/*.php
```

This works for init services, inherited-socket workers and ordinary process pools.
Paths are relative to the **application YAML**, regardless of `directory`;
absolute paths are also accepted. Matching uses the same glob syntax as `watch`:
`*` stays within one component and `**` spans zero or more directory levels.
For example, `src/**/*.js` matches both `src/main.js` and `src/web/routes/main.js`.
Rules apply to future files too. Omitted or empty `events` defaults to the four
shown above. `chmod` is also accepted: Linux reports it separately, while the
Windows backend reports attribute changes as `write`. Rename may also carry
create/remove flags on Linux. Keeping the default set covers atomic file saves.
A matching parent-directory create/remove/rename also invalidates its potential
matching children. Keep watch roots stable; for replaceable subdirectories use
a glob rooted at their persistent parent. No content comparison is performed.

ooth reuses its filesystem watcher and merges matching events per app after a
100 ms quiet period, following the watcher's own 100 ms event aggregation.
The five-second configuration reconciliation bounds the wait during continuous
events. A normal event affects only the matching apps. Reported queue overflow
or event loss logs a warning and conservatively retires active apps that have
restart rules; an exact lost event's type cannot be recovered. Invalid YAML
edits retain the last valid rules. Rules can be added, changed or removed on reload.

For a triggered app, **all current workers receive graceful stop**. They must
stop accepting new work immediately, finish active requests, emit final events
and exit. `stop_timeout` still forces termination when a process does not comply.
ooth keeps its listening socket open, so connections can queue during draining;
it never accepts those connections itself. Retiring processes are excluded from
active capacity and `max_workers`. New demand can start a replacement immediately,
even at `max_workers: 1`, while the old process completes its requests. Thus the
total OS process count can temporarily exceed the maximum of active workers;
each retiring process retains its original stop deadline.
Without `listen`, socket availability during restart is the worker's responsibility.

Replacement follows the existing minimum, startup, dependency and demand rules.
An app with minimum zero and no fresh demand stays at zero after draining;
editing a file never wakes an already dormant app. The previous autoscaled
worker count is not restored automatically. Init services kept alive by
`min_workers`, `startup` or a dependency start replacements during retirement too.
An SCM service has one registered instance and waits for its previous instance
to stop before restarting. Programs owning exclusive resources must release them as part of graceful stop;
otherwise their replacement may need the normal startup retry/backoff.

For example, with `min_workers: 0` and `max_workers: 1`, worker A is answering
a slow request when its source changes. ooth asks A to stop; A closes its own
listener reference and finishes that request. A new connection then activates
worker B on the same parent listener. B loads the updated code and can answer
before A exits. There are temporarily two processes, but only B counts as active.
Without that new connection, no B starts. With `min_workers: 1`, B starts without
waiting for traffic. Failure-triggered replacements retain the restart backoff,
including while the failed process is still stopping.

The runnable [Python example](examples/app1/ooth.yaml) watches `../worker.py`.
Run it as shown above, send a slow request such as `/2000`, save `worker.py`,
then send a second request. The response's worker PID identifies the replacement;
the first request should finish on its original PID, within `stop_timeout`.

Workers load the new source/configuration when the new process starts. Native
Linux/Windows tests in `restart_test.go` cover active draining, fresh cached
contents, retained TCP/Unix listeners, lazy zero, event filtering, atomic saves,
rule replacement, rejected YAML and forced-timeout recovery.

## Process identity

Application YAML may select an account for both socket workers and ordinary services. Omit these fields to inherit ooth's identity:

```yaml
# Linux: names or numeric IDs of existing accounts/groups.
user: www-data
group: www-data
# Optional, for listen.network: unix; default 0660.
socket_mode: 0660
# Equivalent symbolic spelling: socket_mode: "u=rw,g=rw,o="
```

```yaml
# Windows: local name, DOMAIN\user, or user@domain.
user: 'MACHINE\worker'
password: 'account-password'
```

Linux sets the UID, primary GID and account's supplementary groups in the child. The primary group defaults to the account's group. An unprivileged caller retaining its own UID keeps its existing supplementary groups. Changing identity requires the corresponding OS permissions. With CGO disabled, account lookup uses `/etc/passwd` and `/etc/group`, not NSS plugins.

Windows authenticates with `LogonUserW` (batch logon) and passes the primary token through Go's existing `SysProcAttr.Token` to `CreateProcessAsUser`, retaining atomic Job assignment and listener inheritance. A different account requires `password`; an explicit empty string is passed as an empty password. The current account can be named without a password. The target needs the **Log on as a batch job** right, and the caller needs the privileges required by [CreateProcessAsUser](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessasuserw), typically a suitably configured service account. ooth does not grant those rights or retry with its own identity if authentication or creation fails.

The password is read directly from YAML as requested; restrict that file's permissions. It is not placed in the worker's arguments or environment, and YAML diagnostics omit source excerpts. This switches process credentials, without loading a Windows profile or constructing a login environment; use `env` and `directory` for application settings. Linux rejects `password`; Windows rejects `group`. Linux different-user listener inheritance was tested locally; Windows current-user inheritance and authentication failures passed, while a successful different-account Windows spawn still needs validation with a suitable account. `OOTH_TEST_WINDOWS_USER` and `OOTH_TEST_WINDOWS_PASSWORD` enable that optional test only.

The same identity owns the **filesystem Unix socket** used for activation. Linux applies UID/GID and `socket_mode`, defaulting to `0660`; this optional field accepts permission bits `0000` through `0777` and is rejected for TCP or Windows. Windows sets the account as owner and a protected ACL granting full access to that account, ooth's account and SYSTEM. Without an explicit identity, these defaults use ooth's current account/group. TCP listeners have no filesystem owner or mode. An ownership/permission error rejects the configuration; it does not silently expose the socket with different permissions. Identity and mode changes update a reused Unix listener, with permission rollback if the configuration cannot be applied. The socket directory must already allow the intended clients to traverse it and should restrict access during socket creation, before ooth applies the final file permissions. Assigning another Windows owner requires the relevant native ownership rights; ooth does not enable extra privileges automatically.

Windows supports filesystem Unix sockets and enforces their file permissions, as described in Microsoft's [AF_UNIX documentation](https://devblogs.microsoft.com/commandline/af_unix-comes-to-windows/). Local tests verify ACL/owner changes and rollback, stdin inheritance, real request traffic, scaling and graceful shutdown with Go workers. Inheriting the listener avoids creating/binding it in the worker; the worker runtime still needs to adopt its native handle and support `accept` for that socket family.

`socket_mode` accepts an octal YAML integer (`0660`, `0o660`), a quoted octal string (`"0660"`), or symbolic clauses (`"u=rw,g=rw,o="`, `"a+rw"`, `"g-w"`). Symbolic clauses are applied left to right to the fixed default `0660`, so reloads are deterministic and independent of the supervisor's umask or previous file mode. Supported selectors are `u`, `g`, `o`, `a`; each comma-separated clause has one `=`, `+` or `-` operator and `r`, `w`, `x` permissions. Other chmod features, including permission copying and special bits, are rejected.

## Virtual activation and dependencies

An app without `listen` is a virtual activation target. It needs no inherited socket or ooth protocol:

```yaml
name: database
command: [/usr/bin/my-database, --foreground]
ready: tcp://127.0.0.1:5432
```

The first demand for `web` activates `database` and `cache` together. `web` starts after both are ready. Shared prerequisites start once. This is an internal dependency graph, not an additional control socket.

- `ready: started`: base readiness as soon as process creation succeeds; default for all services. Awaited post-start/readiness actions may delay effective readiness further. This orders process creation, not application initialization, and works even with silent stdout.
- `ready: event`: explicitly require the first stdout line to be the `ready` handshake before releasing dependent services. A different first line fails this readiness requirement; silence reaches `start_timeout`.
- `ready: tcp://host:port` or `unix://path`: connect to the endpoint until it responds. Probes establish and close a real connection, so use an endpoint that tolerates that.
- `startup: true`: activate at supervisor startup and keep at least one worker.

Virtual services default to `max_workers: 1`, but can use larger pools. They remain running while required. Without telemetry, retirement follows dependency demand; with telemetry, idle workers retire down to the configured minimum or the one process retained for a required dependency/startup service. Losing a required readiness/started prerequisite stops dependent workers; `dependencies: {name: parallel}` only gates readiness. Demand reactivates the dependency graph. Supervisor shutdown drains dependents before prerequisites. The scheduler does not require unrelated branches to wait for one another.

### Programs that open their own listeners

Omit `listen` and configure the ordinary pool and the worker's own arguments:

```yaml
name: web
vars:
  endpoint: tcp://127.0.0.1:8080
command: [node, /path/to/ooth/tools/probes/node_worker.cjs]
env:
  TEST_BIND_URL: "{{.vars.endpoint}}"
min_workers: 1
max_workers: 4
concurrency: 1
scale_at: 80%
scale_window: 1s
idle_timeout: 1m
ready: event
```

This example uses the repository's Linux Node test worker, which requests
`reusePort` itself. Other programs have their own options. ooth neither opens
a socket nor sets reuseport here; stdout telemetry drives growth and stdin
requests graceful retirement just as for inherited workers. The user must choose
an endpoint, runtime and OS that support multiple independent listeners. Socket
ownership and permissions also belong to the program that creates the socket.

Without an ooth listener, arriving connections cannot activate a zero-sized
pool. Keep `min_workers: 1` (or higher) for an independently reachable service.
The minimum restarts after a crash, with the normal restart backoff; it is not
a guarantee of zero downtime. Setting `min_workers: 1` on an app **with** `listen`
still creates and inherits that listener: these settings control different things.

Linux TCP reuseport listeners have separate accept queues. A connection already
queued on ooth's own listener would not be picked up merely by binding another
socket to its port. The [saved queue probe](tools/probes/reuseport/README.md)
demonstrates this distinction and lists runtime/OS limits. No sysctl or eBPF
migration is configured; Windows `SO_REUSEADDR` and filesystem Unix sockets are
not treated as equivalent reuseport groups.

## Generic worker protocol: spawn, environment and stdio

The convention is language-independent and requires no ooth client library.
ooth starts a foreground process, observes its exit through the operating system,
and optionally reads its request events. With `listen`, it also supplies an
already-listening socket; that worker must adopt it rather than bind a replacement.
Without `listen`, the program owns its listener or other work source. Workers
handle traffic directly and must not daemonize away from the managed process.

### Environment and inherited listener

| Variable | Value and meaning |
| --- | --- |
| `OOTH_WORKER` | `1` for a process started by ooth, including virtual services. |
| `OOTH_LISTEN_HANDLE` | Decimal integer identifying the extra inherited listener: an fd on Linux, a native SOCKET on Windows. Absent for virtual services and legacy stdin handoff. |
| `OOTH_CONCURRENCY` | Decimal configured capacity used by ooth's occupancy calculation. A sizing hint, not an enforced semaphore or a worker count. |

`socket_handoff: env` is the default on both systems. Parse the supplied integer
without truncating a native handle. Linux currently assigns fd 3 by default;
workers should read the variable rather than assume that number. An explicit
Linux `socket_handoff: 7`, for example, assigns fd 7 and advertises `7`.
Windows native handles do not imply CRT descriptor slots such as 3 or 7.

The number alone does not transfer or reopen a socket: inheritance happens at
process creation. Each child owns its inherited reference and may close that
reference when retiring. Other workers and ooth retain their own references.
With `socket_handoff: stdin` (or `0`), adopt fd 0 on Linux or the native
`STD_INPUT_HANDLE` on Windows instead; no environment handle or stdin command
is supplied for that listener. Configuration cannot override `OOTH_*` variables.

### Input and output channels

| Channel | Direction | Use |
| --- | --- | --- |
| Extra listener | Webserver/client → worker | Connections and application traffic; ooth does not read them. |
| stdin | ooth → worker | Line-based stop command after handshake, when stdin is free. With legacy handoff, stdin is the listener instead. |
| stdout | Worker → ooth | First-line protocol detection, then request events; otherwise ordinary process output. |
| stderr | Worker → operator | Diagnostics and application logs; never protocol events. |

### Startup and request events

1. With an inherited listener, read `OOTH_LISTEN_HANDLE`: a file descriptor on Linux or native SOCKET on Windows. Adopt it using the runtime's socket API. With explicit `socket_handoff: stdin`, recover fd 0 / `STD_INPUT_HANDLE` instead. An app without `listen` initializes its own work source.
2. To opt into request telemetry, make the first stdout line the `ready` handshake below. This detection is independent of the configured readiness gate. Send logs to stderr after opting in.
3. Emit `start` and `end` for every request. Flush each complete line. Concurrent workers must serialize writes to their own event stream.
4. After the handshake, read the `v=1 event=stop ts=...` command from stdin, stop accepting new requests, finish active requests, then exit. Handle OS graceful notifications too, for legacy stdin handoff or shutdown before the handshake.

```text
v=1 event=ready ts=1790193600000000000
v=1 event=start ts=1790193600001000000 id=42
v=1 event=end ts=1790193600002000000 id=42 duration_ns=1000000
```

The first complete stdout line must be a valid version-1 `ready` event to enable telemetry. Otherwise stdout is forwarded to ooth's stderr as ordinary output for that worker's lifetime, including any later protocol-looking lines. A silent worker remains supervised without telemetry. Without the handshake there is no request-based growth, idle shrinking or request watchdog; configured minimum workers, cold activation, crash restart and shutdown still work. Virtual dependencies can still stop when no longer required. This default needs no worker changes; `ready: event` opts into the stricter readiness requirement described above.

Emit `ready` exactly once after the listener and handler are initialized. No
banner or log line may precede it. Readiness gates (`started`, `event`, endpoint)
remain separate from this per-process protocol negotiation.

| Event | Required fields | Rule |
| --- | --- | --- |
| `ready` | `v=1 event=ready ts=…` | First complete stdout line; opts into telemetry and, with free stdin, stop commands. |
| `start` | `v=1 event=start ts=… id=…` | Begin one request; ID must not already be active in that process. |
| `end` | `v=1 event=end ts=… id=… duration_ns=…` | Finish that active request, including an aborted/error completion; duration is nonnegative. |

Different requests may overlap and finish in any order. IDs are local to the
worker and may be reused after their end event. Emit a complete line followed by
LF, flush it promptly, and serialize concurrent writers. Version 1 has no extra
status fields, acknowledgements, heartbeats, or accept/queue timestamps.

For ordinary HTTP, one pair describes one request, not a whole keepalive
connection; each HTTP/2 stream has its own pair. The WebSocket examples instead
use one pair for the session lifetime, including time between messages, so an
open session prevents idle retirement. Echo messages are not additional requests.
This is an application convention using the same existing events; adjust
`concurrency` and any request timeout for long-lived sessions.

No JSON or escaping. Fields are space-separated `key=value` pairs; values are printable ASCII without spaces or `=`. Request IDs are unique among active requests within one worker. `ts` is Unix time in nanoseconds; `duration_ns` is elapsed request time in nanoseconds, preferably measured with a monotonic clock. Supervisor deadlines use its own monotonic clock, so worker clock changes cannot extend them. After an accepted handshake, unknown/duplicate fields, oversized lines (4096-byte limit), invalid event order, and a broken event stream stop that worker and enter the restart policy. Ordinary output has no protocol line-length limit.

### Graceful stop and process exit

The command from ooth to a protocol worker is:

```text
v=1 event=stop ts=1790193601000000000
```

ooth sends exactly one stop line on the worker's stdin pipe **only after accepting its stdout handshake**. The handshake now opts into both telemetry and stdin stop handling when stdin is available; this also works for virtual services. ooth sends no simultaneous signal after a successful write. Without a handshake, with listener-on-stdin, or if the pipe write fails, it uses the platform's graceful signal/notification. In either path `stop_timeout` bounds draining and then forces termination. This control protocol lives in `main.go`, not the Go toolchain patch.

On stop, close the worker's own listener reference immediately, stop the accept
loop and refuse new requests on persistent connections too. Do not call socket
`shutdown` on the shared listening socket: other workers and ooth still own it.
Finish active requests,
emit their final `end` lines, flush stdout and exit. The WebSocket examples send
1001 Going Away and complete the close exchange. No `stopped` event exists: the
OS process-exit notification is authoritative. Keeping a pipe or wait thread
alive after draining can still reach the forceful timeout. The worker may also
exit or crash without a command; ooth detects that independently of the protocol
and applies its restart/demand policy. An inherited listener alone does not make
an unmodified runtime capable of interpreting telemetry or graceful commands.

See [examples/worker.py](examples/worker.py) for a minimal implementation and the [runtime matrix](tools/probes/runtime/README.md) for HTTP and WebSocket adapters. Stock PHP-CGI uses `socket_handoff: stdin` and emits no ooth handshake; activation and supervision work, but request-based scaling and idle detection do not. PHP-CGI's `-b` creates a listener rather than importing an environment-provided handle; `FPM_SOCKETS` belongs to PHP-FPM. See the [CGI audit](tools/probes/proxy/README.md#php-cgi-listener-selection).

## Platform details

Both platforms use `syscall.EpollCreate1`, `EpollCtl` and `EpollWait`. One goroutine waits for **all listeners**; there is no goroutine or periodic readiness check per listener. ooth registers `EPOLLIN | EPOLLONESHOT`, then rearms delivered listeners through the supervisor's existing 25 ms tick. This bounds notifications while a connection waits for a starting worker. Unique registration IDs discard events from removed configurations. A private loopback UDP socket wakes the poller during shutdown; it is not a worker control channel.

Linux uses the existing kernel epoll implementation. The old `Read(nil)` change is gone; the only syscall addition on Linux is `EpollClose(int)`, a portable close spelling shared with the Windows extension. When stdin control is unavailable, shutdown sends `SIGTERM`. Both shutdown paths then kill the worker's cgroup after `stop_timeout`, or use `Process.Kill` (`SIGKILL`) when cgroups are unavailable.

Windows needs more than that Unix patch. A connected socket's zero-byte `WSARecv` already waits for data, but a listening socket does not provide that operation. [Issue 15735](https://github.com/golang/go/issues/15735) and the tests in [CL 22031](https://go-review.googlesource.com/c/go/+/22031) concern connections after `Accept`; the comment quoted in [issue 27315](https://github.com/golang/go/issues/27315) points to that proposal. Local tests on stock Go 1.24.3 and 1.27.1 reproduced this distinction.

The Windows toolchain patch:

- Adds the epoll socket API using asynchronous `IOCTL_AFD_POLL` requests and one IOCP per poller. A separate AFD device handle owns the IOCP association; monitored sockets are not attached to it. Requests remain pinned until their completion packets are drained, including cancellation and close. No Rust, C, CGO, libuv or wepoll binary dependency is needed.
- Uses standard Go inheritance (`ExtraFiles` on Linux, `AdditionalInheritedHandles` on Windows, or legacy `Cmd.Stdin`); no socket-inheritance override is needed. The former `StartProcess` override was removed after TCP and Unix lifecycle tests passed without it. Listener initialization first tries normal IOCP. Only when this fails with `ERROR_INVALID_PARAMETER` on a listening socket does it enable Go's existing local-event I/O path, adding deadline/close tracking. This follows libuv's imported-socket fallback approach. The inherited `AcceptEx` fallback checks deadline/close every 20 ms; that worker-side wait is separate from the supervisor's event-driven AFD poller. Connected sockets retain Go's normal I/O implementation.
- Adds the portable `exec.Cmd.NewProcessGroup` option (a no-op on Linux) and supports `Process.Signal(os.Interrupt)`. Visible GUI windows receive `WM_CLOSE`; console groups receive `CTRL_BREAK_EVENT`. A supervisor started without a console allocates a hidden console for its console workers.
- Adds `syscall.SysProcAttr.JobObjects` and passes those handles through `PROC_THREAD_ATTRIBUTE_JOB_LIST` during process creation, before child code can run. Requires Windows 10 / Server 2016 or newer. Job creation, queries and termination live in `job_windows.go`, outside the toolchain patch. No `taskkill`, PowerShell, or shell command is launched by ooth.

Workers without stdin protocol support must handle the OS graceful notification. A GUI may reject `WM_CLOSE`; a fully detached headless process has no universal graceful Windows notification. Such a process reaches the forceful timeout. Explicit SCM services use the separate control path below. Graceful shutdown still asks the foreground worker to drain its work; forced shutdown terminates its whole Job, including descendants.

### Optional Windows SCM support

To run ooth as an already registered **own-process** Windows service, configure
its service command line with absolute paths, for example:

```text
C:\ooth\ooth.exe -service Ooth -config C:\ooth\ooth.yaml -log-file C:\ooth\ooth.log
```

`-service` enters the native SCM dispatcher. It reports start-pending until
ooth has loaded its configuration and bound listeners, then running. SCM stop
and shutdown requests cancel the supervisor and use its normal dependency-aware
graceful shutdown and worker timeouts. While shutting down it reports stop-pending
with progress checkpoints; it reports completion only after the supervisor exits.
Startup/runtime failures produce a nonzero service exit code. `-log-file` appends
logs because service processes generally have no useful stderr. Ordinary console
execution remains the default. Linux rejects `-service`.

An application can instead name another **already registered** Windows service:

```yaml
name: database
scm:
  name: ExampleDatabase
  args: ['{{.vars.mode}}']
vars:
  mode: production
min_workers: 1
start_timeout: 30s
stop_timeout: 15s
restart_on:
  - glob: database-config/**/*.yaml
```

ooth calls `StartService` and `ControlService(STOP)` directly, with no `sc.exe`,
PowerShell or helper process. `scm.args` are arguments to the registered service's
ServiceMain, expanded with the existing placeholders; they are not executable
command-line arguments. Command, environment, working directory and identity
come from the SCM registration, so ooth rejects their per-app overrides here.
Socket inheritance and stdout telemetry are unavailable through SCM.

SCM running status supplies base readiness; awaited post-start/readiness actions
can gate it further. Action commands run as ooth, not the SCM service account.
The normal `startup`, `min_workers`,
dependencies, crash backoff and `restart_on` rules apply, with `max_workers: 1`.
There is no request-based scaling or activation by a parent socket. The same
SCM name cannot appear in two app definitions. A service already active at the
first start attempt is left alone and reported as an error; ooth does not take
ownership of an instance someone else started. Configure SCM recovery separately
so it does not compete with ooth's restart policy.

State changes use native
[service subscriptions](https://learn.microsoft.com/en-us/windows/win32/services/subscribeservicechangenotifications),
not periodic enumeration. Stop timeout terminates the retained service process
handle, protecting against PID reuse. Only own-process services are accepted:
killing a shared service host would also kill unrelated services. A PID may not
yet be valid during start-pending; a force request is retained and applied
when a running/paused state provides a valid PID. Until then forced termination
cannot be guaranteed: SCM does not guarantee a valid PID in pending states,
as documented for [QueryServiceStatusEx](https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-queryservicestatusex).
SCM services are created by Windows outside
ooth's worker Job, so this path does not claim Job-based descendant ownership.

The caller needs service query/start/stop rights and terminate access to the
service process. ooth does not register services, change accounts or recovery
settings, elevate privileges, or convert a console executable into an SCM service.
Registration is an administrator/deployment step; the service executable must
already implement the SCM protocol.

`TestSCMHostGraceful`, `TestSCMHostStartupError`, `TestSCMRetirement` and
`TestSCMNativeSubscription` pass locally, covering the handler, scheduler and a
read-only native subscription. `TestSCMProcessHandleTermination` verifies forceful
termination with a real process and retained handle; `TestSCMPendingPIDIsNotTrusted`
checks that pending-state PIDs are ignored. `TestSCMLifecycle` creates/removes a temporary
service and tests real start, graceful stop and forced termination; it is skipped
in the current non-elevated session because service creation is denied. Run it
from an appropriately privileged Windows test session before treating the full
SCM lifecycle as verified. `x/sys/windows/svc` was already present in the pinned
module graph; it is now a direct dependency, with no new module or CGO.

From an elevated PowerShell in the repository, the native test command is:

```powershell
$env:CGO_ENABLED = '0'
.\build\windows-amd64\go\bin\go.exe test -v -run '^TestSCM' -count=1 -timeout=60s .
```

### Descendant ownership

For directly spawned processes the common API is deliberately small: `processOwner.Start/Wait/Close` and `processJob.Kill/Close/Members`. `Wait` uses `exec.Cmd.Wait` to observe direct-child exits, including crashes, without polling. Before the supervisor removes or restarts a worker, it terminates any remaining descendants and drains cleanup. A worker exiting ends that worker instance: daemonizing children does not keep the service alive. Final stdout events are drained, with a 100 ms deadline for a pipe retained by other processes. SCM services use the distinct ownership rules above.

Windows creates one private, non-inheritable Job handle per worker instance, enables `KILL_ON_JOB_CLOSE`, and permits no breakaway. The process is assigned atomically at creation; descendants inherit membership automatically. `TerminateJobObject` terminates the family. Closing ooth also closes its Job handles and terminates their members. Cleanup waits on remaining process handles, checking Job membership to avoid PID reuse mistakes. See [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects) and [creation-time Job assignment](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute).

Linux creates a cgroup per worker and uses Go's existing `UseCgroupFD/CgroupFD` fields for atomic placement with `clone3`. `cgroup.kill` covers descendants, including nested cgroups; cleanup waits for `cgroup.events` to become empty using epoll. No process-tree scan reconstructs ancestry. PID 1 already adopts orphans; with any other PID, ooth enables `PR_SET_CHILD_SUBREAPER` independently of cgroups. Both cases use `SIGCHLD` to reap current adopted children, excluding managed workers so they cannot steal their `Cmd.Wait` result. If subreaper setup is blocked, a separate warning reports that orphan adoption is unavailable; direct-worker waits and any available cgroup control remain active. The previous once-per-second PID 1 scan is gone. These utilities require no new Linux toolchain patch.

Full Linux ownership needs cgroup v2, `clone3` with `CLONE_INTO_CGROUP` (Linux 5.7+) and `cgroup.kill` (Linux 5.14+), with permission to create subgroups and place processes there. **Root is not required** when a subtree is delegated to the user, for example by a service manager using `Delegate=yes`. Migration permissions also matter when ooth starts outside that subtree. Containers may expose read-only cgroups or block `clone3`. If setup is unavailable, ooth warns and continues with direct children; if placement fails but the command succeeds without placement, it warns and disables placement for subsequent workers. PID 1 reaping and non-PID-1 subreaping remain available independently. Without cgroups, adopting an orphan does not recover its original service membership or provide reliable whole-family termination. See the kernel's [cgroup v2 documentation](https://docs.kernel.org/admin-guide/cgroup-v2.html).

Kernel membership is authoritative; there is no promised stream of every descendant birth/exit. These groups supervise cooperative services, not malicious workers deliberately migrating themselves or launching through unrelated system services. Linux cgroups persist if ooth itself is forcibly killed: automatic recovery of those groups is not implemented. On normal shutdown or a worker crash, ooth cleans the groups before proceeding.

Go workers that share a Windows listener must use this patched toolchain too, including when converting stdin with `net.FileListener`. The [handoff probe](tools/probes/README.md#windows-listener-handoff) refines this requirement: with stock Go 1.27.1 the first worker converts stdin and replies, but the next worker fails during `net.FileListener`, both with concurrent workers and after killing/waiting for the first. The handle is valid; adopting the shared listener into another IOCP fails. Three workers pass with the patched compiler.

Other languages need their native inherited-socket support. PHP's TCP FastCGI accept path retrieves the native handle with `_get_osfhandle` and calls `accept`; that retrieval does not detach an IOCP. libuv already handles failed IOCP association for imported sockets using local events. Local Node.js 24.19.0 probes made three workers accept using its private native-handle binding, but public `listen({fd: 0})` and `listen(process.stdin)` failed on Windows. Node documents that listening on a file descriptor is unsupported there. The private-binding probe is diagnostic, not a supported Node worker adapter. The later proxy suite also ran PHP-CGI: directly on Linux and through the explicit invalid-output-handle launcher on Windows. See the probe notes for sources and reproduction steps.

A further [Node 26.10.0 probe](tools/probes/README.md#node-through-stdin-alone) recovers the socket directly from stdin using built-in `node:ffi` and either `GetStdHandle` or `_get_osfhandle(0)`. Three concurrent workers passed without an additional inherited handle, handle-number environment variable or patched parent. Node still needs its private TCPWrap binding to adopt the Windows socket. The experimental fixture `tools/probes/node_worker.cjs` implements ooth telemetry and graceful draining; set `OOTH_TEST_NODE` to the Node executable to enable its lifecycle test. The current fixture defaults to `OOTH_LISTEN_HANDLE` and handles the stdin stop command; FFI is needed only for legacy Windows stdin handoff. On Linux it uses the public fd API. No Node/npm dependency was added to ooth, and the Windows adapter is not a stable public API.

### Reusable epoll extension

The implementation is in [`tools/patchgo/patches/epoll_windows.txt`](tools/patchgo/patches/epoll_windows.txt), installed as `syscall/ooth_epoll_windows.go`. Its API has no knowledge of workers, configuration or ooth. The AFD mechanism follows the approach demonstrated by [wepoll](https://github.com/piscisaureus/wepoll) and [libuv](https://github.com/libuv/libuv/blob/v1.x/src/win/poll.c); attribution is included in `THIRD_PARTY_NOTICES.md`.

- Supports `EpollCreate`, `EpollCreate1`, `EpollCtl`, `EpollWait` and `EpollClose` on Windows amd64/arm64, for native Winsock sockets including TCP, UDP and Unix sockets.
- Supports ADD/MOD/DEL, level triggering and `EPOLLONESHOT`; events include IN, OUT, PRI, ERR, HUP and RDHUP. Unsupported flags, including `EPOLLET`, are rejected. This is a documented subset, not complete Linux epoll compatibility.
- Each underlying socket has one owning AFD poller. DEL before closing or reusing its handle, including duplicated/inherited handles. One caller performs Wait; control operations may run concurrently. Fd/Pad in `EpollEvent` are opaque application data, not the socket handle; pass the full-width handle as the `EpollCtl` fd argument.
- Use `EpollClose`, not Windows `CloseHandle`, to cancel and drain pending requests safely. AFD structures use an internal Windows interface. The backend is experimental and tested on the supported build matrix; other providers and Windows versions need validation.

`epoll_test.go` exercises readiness without consuming traffic, one-shot rearming, level triggering, pending MOD/DEL/re-add, shutdown cancellation, connection writability/half-close and 128 idle listeners without per-socket goroutine growth. Run `go test -run '^$' -bench BenchmarkEpollIdleListeners -benchmem .` with the dedicated compiler for an idle-queue microbenchmark; it is not an application-throughput comparison.

## Development status

The [stack findings](tools/probes/runtime/FINDINGS.md) record connection reuse, accept distribution, runtime adoption and shutdown behavior, with reproduction paths and explicit unresolved limits. In particular, a growing pool does not redistribute streams on an existing HTTP/2 connection. The lifecycle fixtures deliberately exercise fresh connections; the pressure experiments retain them to expose that limitation.

The opt-in [pressure experiments](tools/probes/pressure/README.md) compare listener notifications, accept observations, request activity and client latency under open-loop load. They include Go/Python/Node/PHP, fixed and growing pools, and Caddy/Nginx upstreams. Generated configurations and raw CSV traces stay under `build/`; the fixtures, analyzer and aggregate results are versioned. These measurements do not change the scaling policy or add events to the worker protocol.

The [Caddy/Nginx suite](tools/probes/proxy/README.md) starts each real webserver through ooth's init mode and tests Python/HTTP, Node/h2c and stock PHP/FastCGI. It passed on Linux and Windows, using the documented PHP launcher on Windows. Python/Node checks cover cold activation, two worker PIDs, start/end telemetry, return to zero after TTL and reactivation; PHP stays at one ordinary worker without telemetry. All worker sources, configuration templates, runtime preparation scripts and test code are committed; downloaded runtimes and generated logs stay under ignored `build/`. This suite fixed a regression where idle workers being stopped could inadvertently trigger replacements.

The supervisor is in `main.go`; platform process utilities are in `job_linux.go` and `job_windows.go`, and toolchain changes in `tools/patchgo`. Integration tests cover descendant ownership across immediate parent exit, forced tree cleanup, crash restart, Windows breakaway rejection, atomic assignment failure and the Linux warning/fallback path. Set `OOTH_CGROUP_ROOT` to a delegated subtree and run Linux tests from within that delegation to enable the cgroup cases.

Integration tests cover cold activation, TCP/Unix listeners, multiple worker processes, idle return to zero, virtual prerequisites, added/reloaded/removed applications, rejected configuration, graceful draining and forced termination. Set `OOTH_TEST_PYTHON` to an interpreter's absolute path to test the Python example as well. The workflow enables that test. The Linux suite also passed `go test -race` locally. Additional [platform probes](tools/probes/README.md) verified actual Linux PID 1 orphan reaping, Windows GUI shutdown and operation without a console or inherited standard handles. These are bounded checks, not a claim of compatibility with every worker/runtime.

Direct dependencies: `sgtdi/fswatcher v1.3.0`, `goccy/go-yaml v1.19.2`, `gopsutil/v4 v4.26.8`, and `golang.org/x/sys v0.45.0`; transitive Go modules are pinned in `go.mod`/`go.sum`. [Canonical Pebble](https://github.com/canonical/pebble) is an architectural reference, not a dependency.
