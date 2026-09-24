# Socketify inherited-listener experiment

`worker.py` uses Python `ctypes` to attach an inherited TCP listener to the actual
Socketify/uWebSockets HTTP server. No Python, Socketify or C/C++ recompilation is
needed. This is a **private ABI experiment**, not an upstream adoption API or a
production ooth adapter. The supervisor never accepts or forwards request data.

## Verified locally

`TestSocketifyExtraHandle` passed on Windows amd64 (CPython 3.14.3) and Linux
amd64/WSL2 (CPython 3.12.3). On each platform:

- Go opens one listener and passes an extra inherited handle to three concurrent
  workers using the existing `ExtraFiles` / `AdditionalInheritedHandles` APIs.
- `OOTH_LISTEN_HANDLE` carries fd 3 on Linux or the native handle on Windows.
  Each worker reads an ordinary line from stdin and reports readiness on stdout.
- Each newly added worker must return its own PID through real Socketify HTTP/1.1
  responses while previous workers are still running.
- Workers exit successfully after an OS interrupt. After closing one worker's
  listener, the surviving workers still serve requests through their copies.

That original extra-handle probe remains unchanged in scope. With
`TEST_OOTH_PROTOCOL=1`, the same worker now implements ready/start/end telemetry,
stdin stop and asynchronous HTTP handlers. The [common lifecycle matrix](../runtime/README.md)
verifies actual ooth cold activation, growth, responses from both PIDs, idle zero,
reactivation and active-request draining, directly and through Caddy/Nginx on both
OSes. Native Socketify WebSockets pass upgrade, text/binary echo, ping/pong,
survival beyond idle TTL and client/server close exchanges. One event pair spans
each WebSocket session.

Stop closes the listener before stopping the event loop, allowing outstanding
handlers to finish. `/busy600` deliberately blocks the loop during the growth
probe; normal delay and shutdown requests are asynchronous. The probe connects
while the old worker is still busy and does not assume fair serial accepts.
See [distribution findings](../runtime/FINDINGS.md). TLS, Unix sockets and HTTP/2
remain outside this adapter; it does not supply h2c. No production configuration,
Go dependency or native binary changed. Test-only dependencies stay under `tmp/`.

## How the workaround works

The shipped library exports `us_socket_context`, `us_socket_context_loop`, and
the `us_poll_*` functions. It has no ready-made listener-adoption function exposed
by Socketify. The fixture:

1. Lets Socketify create its ordinary HTTP context and a temporary loopback
   listener on port 0, before starting the application's event loop.
2. Retrieves that listener's existing uSockets poll object and context through
   native calls. It reads the private `us_poll_t.uv_p` pointer.
3. Stops the poll and pumps libuv once to complete the queued `uv_close` callback.
   This uSockets implementation retains the poll allocation when its data is null.
4. Closes the temporary socket, then reinitializes the poll with the inherited
   handle and the native listener type. It restores `uv_handle_t.data`, which
   uSockets cleared during stop, and starts readable polling again.
5. Runs normal Socketify. Its native accept loop, HTTP parser, routing and response
   implementation now operate on ooth's original listener.

There is a temporary bind, but the temporary socket is closed before readiness.
This is not a traffic relay and Python does not implement HTTP or accept here.
The `ctypes` access assumes the pinned libuv-backed binary layout; the worker
checks its SHA256 before touching it. Do not generalize this to arbitrary builds,
other event backends, TLS contexts or a running server with active requests.

Unlike the TrueAsync function-pointer hook, this workaround reinitializes an
existing poll and writes its data pointer. A small upstream listener-adoption
entry point would avoid this dependency on internal layout. No upstream patch
or issue has been submitted.

## Reproduce

From the repository root, obtain the Python source and its already-built native
libraries. Submodules do not need to be compiled for this experiment:

```sh
git clone https://github.com/cirospaciari/socketify.py.git tmp/socketify-source
git -C tmp/socketify-source checkout d35e41921dadcd8e27679b7300b51e0a697bfcba
```

If that directory already exists, use the existing checkout and verify its commit
instead of cloning over it. Socketify identifies this source as version 0.0.31.

Windows PowerShell (set `$python` to the desired 64-bit interpreter):

```powershell
$python = (Get-Command python).Source
& $python -m pip install --only-binary=:all: --target tmp/socketify-deps-windows cffi==2.1.1 pycparser==3.0 setuptools==84.0.0
$env:PYTHONPATH = "$PWD/tmp/socketify-source/src;$PWD/tmp/socketify-deps-windows"
$env:OOTH_TEST_SOCKETIFY = $python
$env:CGO_ENABLED = '0'
./build/windows-amd64/go/bin/go.exe run ./test/run.go test -v -run '^TestSocketifyExtraHandle$' .
```

Linux:

```sh
python3 -m pip install --only-binary=:all: --target tmp/socketify-deps-linux cffi==2.1.1 pycparser==3.0 setuptools==84.0.0
PYTHONPATH="$PWD/tmp/socketify-source/src:$PWD/tmp/socketify-deps-linux" \
OOTH_TEST_SOCKETIFY=python3 CGO_ENABLED=0 \
  ./build/linux-amd64/go/bin/go run ./test/run.go test -v -run '^TestSocketifyExtraHandle$' .
```

The Linux binary needs `libuv.so.1` and `libz.so.1`. In the local Ubuntu 24.04 test,
zlib was already installed; libuv was extracted privately without a system install:

```sh
mkdir -p tmp/socketify-libuv
(cd tmp/socketify-libuv && apt download libuv1t64=1.48.0-1.1build1)
dpkg-deb -x tmp/socketify-libuv/libuv1t64_1.48.0-1.1build1_amd64.deb tmp/socketify-libuv
export LD_LIBRARY_PATH="$PWD/tmp/socketify-libuv/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
```

Use the distribution's matching library if already available; the above package
name/version is specific to that Ubuntu release. The Windows DLL includes libuv.

Pinned native SHA256 values:

| Binary | SHA256 |
| --- | --- |
| `libsocketify_windows_amd64.dll` | `b52b1663e6817ad0af15fcd5021c61a6c5898dba247acb5bc419bb3ddf56b750` |
| `libsocketify_linux_amd64.so` | `687d24ed90ae7e67ff3b350904d95db180e42bec6216a7414051015c71b95ea5` |

Source references:

- [Socketify Python/native binding](https://github.com/cirospaciari/socketify.py/tree/d35e41921dadcd8e27679b7300b51e0a697bfcba/src/socketify).
- [uSockets libuv poll lifecycle](https://github.com/cirospaciari/uSockets/blob/0b95ddfa4971a8e90d2f5f9e33b5317985dceaa1/src/eventing/libuv.c).
- [uSockets libuv poll layout](https://github.com/cirospaciari/uSockets/blob/0b95ddfa4971a8e90d2f5f9e33b5317985dceaa1/src/internal/eventing/libuv.h).
- [libuv poll-handle API](https://docs.libuv.org/en/v1.x/poll.html).
