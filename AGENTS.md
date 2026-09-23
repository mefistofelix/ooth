

## Engineering preferences

These principles apply across languages, runtimes, and frameworks.

### Simplicity and readability

- Optimize for cognitive simplicity first and line count second.
- Keep code minimal and readable. Do not use code golf or compress unrelated operations onto one line.
- Let each line express one clear idea.
- Prefer guard clauses for empty, error, and exceptional cases so the main path stays flat.
- Prefer structured data transformations over clever string manipulation.
- Use descriptive names. Do not introduce short aliases solely to reduce repetition.

### Native constructs and dependencies

- Prefer direct language, runtime, and standard-library features over wrappers or custom replacements.
- Do not introduce third-party dependencies without explicit approval.
- Rely on native validation, errors, and defaults instead of reimplementing them.
- Keep the origin of an operation visible when qualification improves clarity. Avoid redundant qualification when the imported symbol already communicates its origin.
- Do not hide ordinary public fields or attributes behind trivial accessors or properties.
- In languages with type inference, avoid redundant annotations. Keep types where required or where they clarify a public boundary.

### Abstractions and boundaries

- Add helpers, abstractions, validation layers, comments, or documentation only when they solve a concrete problem.
- Prefer a few obvious duplicated lines over an abstraction that hides semantics.
- Keep abstraction boundaries strict: lower layers expose generic representations; higher layers translate domain- or protocol-specific concepts.
- Generic parsers and formatters must not branch on concrete drivers unless that syntax belongs to the generic layer.
- At protocol boundaries, prefer a structured argument when a low-level function signature would otherwise keep growing.
- Assume established internal data contracts. Do not add machinery for every theoretical malformed input.

### Errors and concurrency

- Handle errors when they affect program logic or when additional context is actionable. Otherwise, let native errors propagate.
- Do not add broad catches, defensive checks, or fallback behavior merely to suppress possible errors.
- Do not wrap synchronous or blocking APIs in asynchronous functions unless real concurrency is required.
- When blocking work must coexist with asynchronous code, make the boundary explicit at the caller using the language's standard thread or executor mechanism.

## ooth project decisions and state

- Keep all application code in root `main.go`, approximately 1500 lines (a guideline, not a hard cap). Tests, examples and the toolchain patcher are separate. Do not reintroduce application platform files or internal packages without discussing the change.
- The user explicitly approved `github.com/sgtdi/fswatcher` and `github.com/goccy/go-yaml v1.19.2`, including fswatcher's Go dependencies. No other application dependency is approved.
- Build with `CGO_ENABLED=0`. Target Linux and Windows, TCP and Unix sockets. Runtime supervision must not launch management shell commands.
- The parent MUST NOT accept, proxy or copy application traffic. It owns and waits on a listener, passes it through stdin to foreground workers, and receives only events on stdout. Logs go to stderr.
- Worker protocol is line-based ASCII `key=value`, version 1: `ready`, `start` and `end`; Unix-nanosecond timestamps, request IDs and nanosecond durations. No JSON and no separate worker control channel.
- Graceful shutdown uses signals/OS notifications followed by `Process.Kill` after `stop_timeout`. Linux uses SIGTERM. Windows uses WM_CLOSE for windows or CTRL_BREAK for a console group, including a hidden console when needed. Detached headless workers have no universal graceful operation. No Windows SCM integration or descendant-tree termination is implemented.
- Dependencies use internal virtual activation: a validated DAG, lazy recursive demand, parallel independent branches, readiness gates, and reverse dependency shutdown. Programs without socket activation can use process-start or endpoint-probe readiness.
- User approved using the public `syscall.Epoll*` API directly on both platforms, extending it on Windows with AFD/IOCP. Keep this generic backend inside this repository's toolchain patch, separate from supervisor semantics. Current pinned compiler: Go 1.27.1, downloaded into `build/` by `build.sh`; never modify a global toolchain. When patch sources change, build.sh restores all affected stock source files from the verified archive before applying the new version.
- One goroutine and one epoll instance observe all listeners. Registrations use EPOLLIN|EPOLLONESHOT, unique IDs, and rearm on the manager's existing 25 ms tick after delivery. An internal loopback UDP wake socket terminates the blocking wait; it is not a worker control channel. There is no per-listener goroutine or WSAPoll loop. The Read(nil) wait and its Unix patch were removed.
- The Windows syscall extension supports sockets, ADD/MOD/DEL, level triggering, ONESHOT and IN/OUT/PRI/ERR/HUP/RDHUP events. EPOLLET and other unsupported flags are rejected. One owning AFD poller per underlying socket is an accepted user constraint; duplicated/inherited handles do not authorize additional registrations. DEL before closing/reusing a socket. One Wait caller; Ctl may run concurrently. EpollEvent Fd/Pad are opaque data, while EpollCtl takes the full-width handle. EpollClose cancels/drains native requests before unpinning buffers; Linux has the same close spelling as a small wrapper around Close.
- AFD uses a separate device handle and IOCP; it never binds the monitored sockets to its completion port. Implementation references are wepoll and libuv, with wepoll attribution in THIRD_PARTY_NOTICES.md. No Rust, C, CGO or new binary/library dependency was introduced. Do not claim full epoll compatibility or measured application-throughput improvements.
- Windows Read(nil) on an accepted connection and Read(nil) on a listening socket are different. Go issues 15735/27315 and CL 22031 do not establish that WSARecv on a listener waits for incoming connections. Stock Go 1.24.3/1.27.1 probes confirmed this locally. Windows Go workers sharing a listener need the patched toolchain as well.
- Audit confirmed standard Cmd.Stdin inheritance works: the former syscall.StartProcess socket-inheritance override was removed and TCP/Unix lifecycle tests pass. Listener initialization now tries normal IOCP first; only ERROR_INVALID_PARAMETER on a listening socket enables the existing Go event-based I/O path with patched deadline/close tracking. Stock Go 1.27.1 handoff probes show the first worker accepts, then the second fails net.FileListener, even after killing/waiting for the first. Do not claim stdin conversion itself fails or every individual stock Go worker fails. Three patched workers pass. Only the inherited AcceptEx fallback still checks deadline/close every 20 ms; distinguish that worker-side behavior from the supervisor's asynchronous AFD poller. Stock Process.Kill works; stock Windows Process.Signal(os.Interrupt) remains unsupported. Cmd.NewProcessGroup wraps existing SysProcAttr.CreationFlags plus hidden-console setup to keep main.go portable.
- PHP's TCP FastCGI source retrieves a native socket with _get_osfhandle and uses accept; this is handle retrieval, not IOCP reassociation. PHP was inspected, not executed. libuv already falls back to local completion events for imported sockets. On Node 24.19.0/libuv 1.52.1, three workers passed using a diagnostic private TCPWrap binding and an extra inherited native handle; public listen({fd: 0}) and listen(process.stdin) failed as expected from Node's Windows limitation. Do not promote the private Node binding or extra handle/environment into the production worker convention without an explicit design decision. Reproduction is in tools/probes.
- Node 26.10.0/libuv 1.52.1 subsequently passed with stdin alone: built-in node:ffi calls GetStdHandle or _get_osfhandle(0), then private TCPWrap adopts the socket. Both probes served requests from three workers with a stock Go parent, no extra handle/environment, no npm dependency or Node patch. This remains an experimental/private API fixture under tools/probes, not a supported production adapter. OOTH_TEST_NODE enables TestNodeWorker: cold activation, idle replacement and active-request draining with a graceful-exit marker. Linux uses the public fd API; Windows requires the FFI-capable Node runtime. Portable test runtimes stay under ignored build/, verified against official archive checksums; do not install globally or add them as ooth dependencies.
- epoll_test.go checks traffic remains unconsumed, level/oneshot behavior, pending MOD/DEL/re-add, concurrent registration, cancel/drain on close, OUT/RDHUP, and 128 idle listeners without per-socket goroutine growth. BenchmarkEpollIdleListeners measures nonblocking wait overhead, not end-to-end throughput.
- Linux PID 1 reaps adopted zombies through `/proc`, skipping managed workers to avoid racing Cmd.Wait. It requires procfs; it is not a complete machine bootstrap implementation.
- New requirement under discussion: recursively track and stop descendants of supervised processes, including unexpected exits and orphaned descendants. This is not implemented. Cmd.Wait already observes direct child exits on both platforms, followed by draining telemetry (100 ms before forcibly closing a retained stdout pipe); PID 1 orphan scanning is not full descendant tracking or subreaper support outside PID 1. Windows Job Objects with IOCP notifications and Linux subreaper/cgroup v2 are candidate mechanisms, not implemented or finalized decisions. Do not claim subreaper alone reports every descendant birth, or Job Object notifications guarantee delivery of every event.
- The user clarified that descendant ownership must survive a parent exiting immediately; periodic process-tree scans are unacceptable. Prioritize kernel-maintained membership established at process creation (Windows Job Object with no breakaway; Linux cgroup v2) rather than reconstructing ownership from observed events. Linux SysProcAttr.UseCgroupFD/CgroupFD already supports atomic placement; the pinned Windows SysProcAttr lacks a job-list field, although Windows exposes PROC_THREAD_ATTRIBUTE_JOB_LIST. Linux subreaping is a separate orphan/wait concern, not a replacement for membership. Full guarantees require the necessary OS support and cgroup permissions; do not silently degrade to tree polling. This design is still pending implementation.
- Root `src/` and `golang_patch/` retain the historical prototype and patch as separate modules. They are not the current build target.
- Run `bash ./build.sh` on Linux or Git Bash on Windows. It runs native tests/vet and emits amd64/arm64 binaries for the host OS. `OOTH_TEST_PYTHON` enables external-language integration tests. Windows CPython is tested with TCP; its AF_UNIX accept path is unavailable. ARM64 is cross-compiled, not runtime-tested here.
- Development validation is local. Do not run GitHub builds or publish releases for every change, and do not add push/PR/scheduled CI triggers. The user wants manual GitHub verification at agreed milestones; do not automatically dispatch the workflow after ordinary commits. The workflow is workflow_dispatch-only and tests both OSes. Its publish_release boolean defaults to false; enable it only when publishing a milestone prerelease with binaries and SHA256SUMS is intended. README describes configuration, protocol, patch requirements and limitations. Repository: https://github.com/mefistofelix/ooth (public, as explicitly requested by the user).
- Local validation also passed the Linux race detector, actual namespace PID 1 orphan reaping, graceful/forceful shutdown without a Windows console or inherited standard handles, and a WinForms FormClosing handler invoked by WM_CLOSE. Reproducible manual probes are under `tools/probes`. Only visible GUI windows are sent WM_CLOSE: framework-internal hidden windows must not receive it.
