"""Pinned Socketify ctypes experiment; not a supported listener-adoption API."""
import ctypes
import hashlib
import os
from pathlib import Path
import signal
import socket
import sys
import asyncio
import threading
import time

from socketify import App
from socketify.native import ffi, lib, library_path


def adopt_listener(app, handle):
    # These shipped x64 binaries use the libuv us_poll_t layout inspected in
    # uSockets 0b95ddf. Never apply the pointer access to an arbitrary build.
    hashes = {
        "win32": "b52b1663e6817ad0af15fcd5021c61a6c5898dba247acb5bc419bb3ddf56b750",
        "linux": "687d24ed90ae7e67ff3b350904d95db180e42bec6216a7414051015c71b95ea5",
    }
    if hashlib.sha256(Path(library_path).read_bytes()).hexdigest() != hashes.get(sys.platform):
        raise RuntimeError("This ctypes probe requires the documented Socketify binary")
    native = ctypes.CDLL(library_path)
    pointer = ctypes.c_void_p
    descriptor = ctypes.c_size_t if os.name == "nt" else ctypes.c_int
    for name, result, arguments in [
        ("us_socket_context", pointer, [ctypes.c_int, pointer]),
        ("us_socket_context_loop", pointer, [ctypes.c_int, pointer]),
        ("us_poll_fd", descriptor, [pointer]),
        ("us_poll_stop", None, [pointer, pointer]),
        ("us_poll_init", None, [pointer, descriptor, ctypes.c_int]),
        ("us_poll_start", None, [pointer, pointer, ctypes.c_int]),
    ]:
        function = getattr(native, name)
        function.restype = result
        function.argtypes = arguments

    app.listen({"host": "127.0.0.1", "port": 0}, lambda config: None)
    poll = int(ffi.cast("uintptr_t", app.socket))
    temporary = native.us_poll_fd(poll)
    context = native.us_socket_context(0, poll)
    loop = native.us_socket_context_loop(0, context)
    uv_poll = pointer.from_address(poll).value
    if descriptor.from_address(poll + ctypes.sizeof(pointer)).value != temporary:
        raise RuntimeError("Unexpected us_poll_t layout")
    if pointer.from_address(uv_poll).value != poll:
        raise RuntimeError("Unexpected uv_poll_t data pointer")

    # stop queues uv_close but retains the allocation when data is null. Run
    # its close callback before reinitializing that allocation on the same loop.
    native.us_poll_stop(poll, loop)
    app.loop.uv_loop.run_nowait()
    socket.close(temporary)
    native.us_poll_init(poll, handle, 2)  # POLL_TYPE_SEMI_SOCKET: listener
    pointer.from_address(uv_poll).value = poll  # restore uv_handle_t.data
    native.us_poll_start(poll, loop, 1)  # UV_READABLE
    return temporary


protocol = os.environ.get("TEST_OOTH_PROTOCOL") == "1"
if not protocol and sys.stdin.readline() != "ordinary stdin\n":
    raise RuntimeError("Ordinary stdin is not usable")
handle = int(os.environ["OOTH_LISTEN_HANDLE"])
app = App()
active = 0
sequence = 0
stopping = False
websockets = {}


def event(kind, **fields):
    values = {"v": 1, "event": kind, "ts": time.time_ns(), **fields}
    print(" ".join(f"{key}={value}" for key, value in values.items()), flush=True)


async def request_handler(response, request):
    global active, sequence
    path = request.get_url().strip("/")
    blocking = path.startswith("busy")
    delay = min(2000, max(0, int(path.removeprefix("busy") or "0")))
    sequence += 1
    request_id = sequence
    active += 1
    started = time.monotonic_ns()
    event("start", id=request_id)
    try:
        response.on_aborted(lambda _: None)
        if blocking:
            # Deliberately occupy the event loop in the growth probe so a new
            # worker must accept pending connections; serial accept fairness
            # is not guaranteed by the kernel or libuv.
            time.sleep(delay / 1000)
        else:
            await asyncio.sleep(delay / 1000)
        response.end(f"worker={os.getpid()} request={request_id} protocol=http1\n", True)
    finally:
        event("end", id=request_id, duration_ns=time.monotonic_ns() - started)
        active -= 1
        if stopping and active == 0:
            app.loop.loop.call_soon(app.close)


if protocol:
    app.get("/*", request_handler)

    def ws_open(ws):
        global sequence, active
        sequence += 1
        active += 1
        websockets[int(ffi.cast("uintptr_t", ws.ws))] = (ws, sequence, time.monotonic_ns())
        event("start", id=sequence)

    def ws_close(ws, code, message):
        global active
        _, request_id, started = websockets.pop(int(ffi.cast("uintptr_t", ws.ws)))
        event("end", id=request_id, duration_ns=time.monotonic_ns() - started)
        active -= 1
        if stopping and active == 0:
            app.loop.loop.call_soon(app.close)

    app.ws("/ws", {"open": ws_open, "message": lambda ws, message, opcode: ws.send(message, opcode), "close": ws_close})
else:
    app.get("/", lambda response, request: response.end(f"{os.getpid()}\n"))
temporary = adopt_listener(app, handle)
print(f"adopted inherited={handle} closed_temporary={temporary}", file=sys.stderr, flush=True)

def stop(signum=None, frame=None):
    global stopping
    if stopping:
        return
    stopping = True
    for ws, _, _ in list(websockets.values()):
        ws.end(1001, "ooth stopping")
    # App.close also stops its loop. Close only the listener while handlers drain.
    if app.socket != ffi.NULL:
        lib.us_listen_socket_close(app.SSL, app.socket)
        app.socket = ffi.NULL
    if active == 0:
        app.close()


signal.signal(signal.SIGTERM, stop)
if hasattr(signal, "SIGBREAK"):
    signal.signal(signal.SIGBREAK, stop)
if protocol:
    def control():
        for line in sys.stdin:
            fields = dict(field.split("=", 1) for field in line.split())
            if fields.get("v") == "1" and fields.get("event") == "stop":
                app.loop.loop.call_soon_threadsafe(stop)
                return

    threading.Thread(target=control, daemon=True).start()
    event("ready")
else:
    print("ready", flush=True)
app.run()
app.loop.uv_loop.run_nowait()
if not protocol:
    print("closed", flush=True)
