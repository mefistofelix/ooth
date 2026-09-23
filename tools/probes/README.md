# Manual platform probes

Run these from the repository root after `bash ./build.sh`. They use the dedicated patched compiler and OS/interpreter facilities; the handoff comparison also uses a separate stock compiler. The Go files have `ignore` build tags because they are standalone probes, not application packages.

Linux (requires Python 3, `unshare`, user namespaces and procfs):

```sh
python3 tools/probes/pid1.py
CGO_ENABLED=1 ./build/linux-amd64/go/bin/go test -race -timeout 60s .
```

The first probe creates a real PID namespace with ooth as PID 1, starts a process that creates an orphan, checks that no zombie remains and stops ooth with SIGTERM. It cleans up the namespace on failure.

`TestSubreaperWithoutCgroup` covers the other case: an ordinary non-PID-1 process with no usable cgroup adopts a living orphan, then collects its exit through SIGCHLD without a test-side Wait. Closing the owner restores the previous subreaper setting. Subreaping is independent of cgroups; it does not recover the orphan's original service membership.

Descendant ownership tests are part of the root Go suite. On Linux, set `OOTH_CGROUP_ROOT` to a delegated cgroup v2 and run the tests from that delegation; root is not required. `TestProcessJobOwnsOrphans` checks that a grandchild remains in its kernel group after its parent and optionally its root exit immediately, and that cleanup removes the surviving process and reaps zombies. `TestSupervisorCrashCleansDescendants` checks cleanup before restart. Without a writable cgroup the full ownership tests skip (an explicitly requested but unavailable test delegation fails); `TestCgroupUnavailableWarnsAndRuns` verifies the documented warning and direct-child fallback.

For the separate migration-permission case, `OOTH_TEST_UNDELEGATED_GROUP` points at a cgroup whose directory and control files are writable to the test user, but whose common ancestor with the caller's cgroup does not permit migration. Run only `TestCgroupPlacementFallback` from outside that delegation. It verifies that failed atomic placement can fall back to a fresh command and emit the warning; Go commands cannot be started a second time after a failed Start.

Windows ownership tests also check rejected breakaway, invalid creation-time Job assignment without running child code, and kernel cleanup when the Job-owning process exits without calling Close. Job utilities remain in application platform files; only creation-time assignment needs the new stdlib spawn field.

Windows, from PowerShell (the GUI probe uses the installed .NET Framework C# compiler):

```powershell
& "$env:WINDIR/Microsoft.NET/Framework64/v4.0.30319/csc.exe" '/nologo' '/target:winexe' '/reference:System.Windows.Forms.dll' '/reference:System.Drawing.dll' "/out:$PWD\build\GuiProbe.exe" "$PWD\tools\probes\GuiProbe.cs"
& ./build/windows-amd64/go/bin/go.exe run tools/probes/probe_gui.go
& ./build/windows-amd64/go/bin/go.exe test -c -ldflags '-H=windowsgui' -o bin/ooth-no-console.test.exe .
& ./build/windows-amd64/go/bin/go.exe run tools/probes/probe_detached.go
```

The GUI is transparent and minimized. Its FormClosing handler records receipt of WM_CLOSE before exit. The detached probe runs both graceful and forceful worker tests with no inherited standard handles and no console. These probes do not install services or alter the user's global Go installation.

## Windows listener handoff

`socket_handoff_windows.go` starts three workers sharing a TCP listener through standard input. The parent keeps the listening socket and an observation duplicate, as ooth does, but never accepts connections. Each worker must report ready and answer with its own PID; earlier workers remain running. Children have a ten-second deadline and are killed and waited for during cleanup. These are socket adoption probes, not full ooth protocol workers or throughput benchmarks.

Compile the same source with stock Go 1.27.1 and the dedicated patched Go. Set `$stockGo` to a separate, unmodified installation:

```powershell
$stockGo = 'C:/path/to/stock-go/bin/go.exe'
$env:CGO_ENABLED = '0'
$env:GOTOOLCHAIN = 'local'
& $stockGo build -o build/handoff-stock.exe tools/probes/socket_handoff_windows.go
& ./build/windows-amd64/go/bin/go.exe build -o build/handoff-patched.exe tools/probes/socket_handoff_windows.go
& ./build/handoff-stock.exe ./build/handoff-stock.exe
& ./build/handoff-patched.exe ./build/handoff-patched.exe
```

The stock failure is intentional evidence: locally, the first worker replied, but the second failed in `net.FileListener` with Windows error 87, despite stdin being a valid listening socket. All three patched workers replied. To reproduce the stock failure after the first worker has already exited:

```powershell
$env:HANDOFF_SEQUENTIAL = '1'
try {
  & ./build/handoff-stock.exe ./build/handoff-stock.exe
} finally {
  Remove-Item Env:HANDOFF_SEQUENTIAL
}
```

For Node, set `$node` to the installed executable. Locally this was Node 24.19.0 with libuv 1.52.1:

```powershell
$node = 'C:/path/to/node.exe'
& ./build/handoff-stock.exe $node tools/probes/node_handoff.cjs fd
& ./build/handoff-stock.exe $node tools/probes/node_handoff.cjs stdin
$env:HANDOFF_RAW = '1'
try {
  & ./build/handoff-stock.exe $node tools/probes/node_handoff.cjs native
} finally {
  Remove-Item Env:HANDOFF_RAW
}
```

Both public Node paths failed with `EISDIR`. The native mode passed with three worker PIDs. **Native mode is diagnostic only**: it adds a known inherited socket handle through `AdditionalInheritedHandles`, passes its number in `HANDOFF_HANDLE`, and calls the private `process.binding('tcp_wrap')` API. It establishes that libuv can adopt this shared socket without a patched Go parent; it does not establish that Node's public API implements the production stdin convention. No new dependency or worker protocol extension was added to ooth.

### Node through stdin alone

Node 26 adds built-in [`node:ffi`](https://nodejs.org/api/ffi.html), allowing the worker to recover the native stdin socket directly. With Node 26.10.0/libuv 1.52.1, both paths below passed with three concurrent worker PIDs and a stock Go parent:

```powershell
$node = 'C:/path/to/node-v26.10.0-win-x64/node.exe'
& ./build/handoff-stock.exe $node tools/probes/node_handoff.cjs stdhandle
& ./build/handoff-stock.exe $node tools/probes/node_handoff.cjs osfhandle
```

Leave `HANDOFF_RAW` unset. `stdhandle` calls `GetStdHandle(STD_INPUT_HANDLE)` from `kernel32.dll`; `osfhandle` calls `_get_osfhandle(0)` from `ucrtbase.dll`. Both then pass the native socket to Node's private TCPWrap binding, which calls libuv. These modes require **no additional inherited handle, handle-number environment variable, IPC channel, npm dependency or Node patch**. Additional handle inheritance itself is a standard Windows/Go facility; the private part is Node's socket adoption API. FFI remains experimental, and TCPWrap is private, so these are compatibility probes, not a stable Node adapter. Node 24.19.0 lacks the built-in FFI module.

`node_worker.cjs` exercises the actual ooth protocol over stdout and serves HTTP through stdin's listener. It handles SIGTERM on Linux and SIGBREAK on Windows, closes its listener, and lets active responses finish. `TestNodeWorker` verifies cold activation, idle shutdown and replacement, and draining a request during supervisor shutdown. A marker written only by the graceful handler distinguishes a cooperative exit from a forced kill.

```powershell
$env:OOTH_TEST_NODE = $node
& ./build/windows-amd64/go/bin/go.exe test -run '^TestNodeWorker$' -v -timeout 30s .
```

On Linux the same fixture uses public `server.listen({fd: 0})`, without FFI or TCPWrap:

```sh
OOTH_TEST_NODE=/absolute/path/to/node ./build/linux-amd64/go/bin/go test -run '^TestNodeWorker$' -v -timeout 30s .
```

The lifecycle test passed locally on Windows amd64 and Linux amd64 in WSL, with Node 26.10.0. It is skipped unless `OOTH_TEST_NODE` is set. The runtime is a test tool, not an ooth dependency. Local portable archives were downloaded from nodejs.org into ignored `build/runtime-node/` and verified against the official `SHASUMS256.txt`; no system installation or CI trigger was changed. The runtime execution checks here cover Node, not Bun or Deno.

Source audit:

- [Microsoft `_get_osfhandle`](https://learn.microsoft.com/en-us/cpp/c-runtime-library/reference/get-osfhandle?view=msvc-170) retrieves the native handle associated with a CRT file descriptor. It does not create a new socket or move one between IOCPs.
- [PHP FastCGI](https://github.com/php/php-src/blob/master/main/fastcgi.c), TCP branch of `fcgi_accept_request`, uses `(SOCKET)_get_osfhandle(req->listen_socket)` followed by `accept`. Its Windows startup detection also has named-pipe conventions; source inspection alone does not prove unchanged php-cgi works with ooth's inherited TCP listener. PHP was not executed in this check.
- [libuv TCP on Windows](https://github.com/libuv/libuv/blob/v1.x/src/win/tcp.c), `uv__tcp_set_socket`, tries `CreateIoCompletionPort` and sets `UV_HANDLE_EMULATE_IOCP` on failure for an imported socket. Its event/threadpool path handles completion locally. ooth's Go patch follows this conditional approach, retaining the existing Go local-event path and its explicit cancellation/deadline handling.
- [Node `server.listen(handle)`](https://nodejs.org/api/net.html#serverlistenhandle-backlog-callback) explicitly excludes listening on a file descriptor on Windows. [TCPWrap::Open](https://github.com/nodejs/node/blob/v24.19.0/src/tcp_wrap.cc) calls `uv_tcp_open` with the supplied number as the native socket, which is not a CRT fd-to-handle conversion.

This Windows-only check does not alter Linux listener inheritance. The normal Linux worker can still use stock `net.FileListener(os.Stdin)`.
