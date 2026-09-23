

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
- User approved extending the Go toolchain patch for Windows to preserve one portable application file. Current pinned compiler: Go 1.27.1, downloaded into `build/` by `build.sh`; never modify a global toolchain. `tools/patchgo` installs version-checked patches for Unix zero-length wait, Windows listener readiness/sharing, process groups and graceful notifications.
- Windows Read(nil) on an accepted connection and Read(nil) on a listening socket are different. Go issues 15735/27315 and CL 22031 do not establish that WSARecv on a listener waits for incoming connections. Stock Go 1.24.3/1.27.1 probes confirmed this locally. Windows Go workers sharing a listener need the patched toolchain as well.
- Windows readiness currently checks close/deadline state every 20 ms using native waits; account for its OS-thread cost. Do not describe this as an IOCP readiness event or a patch-free implementation.
- Linux PID 1 reaps adopted zombies through `/proc`, skipping managed workers to avoid racing Cmd.Wait. It requires procfs; it is not a complete machine bootstrap implementation.
- Root `src/` and `golang_patch/` retain the historical prototype and patch as separate modules. They are not the current build target.
- Run `bash ./build.sh` on Linux or Git Bash on Windows. It runs native tests/vet and emits amd64/arm64 binaries for the host OS. `OOTH_TEST_PYTHON` enables external-language integration tests. Windows CPython is tested with TCP; its AF_UNIX accept path is unavailable. ARM64 is cross-compiled, not runtime-tested here.
- README describes configuration, protocol, patch requirements and limitations. CI builds/tests both OSes on push; workflow_dispatch publishes prerelease binaries and SHA256SUMS. Repository: https://github.com/mefistofelix/ooth (public, as explicitly requested by the user).
- Local validation also passed the Linux race detector, actual namespace PID 1 orphan reaping, graceful/forceful shutdown without a Windows console or inherited standard handles, and a WinForms FormClosing handler invoked by WM_CLOSE. Reproducible manual probes are under `tools/probes`. Only visible GUI windows are sent WM_CLOSE: framework-internal hidden windows must not receive it.
