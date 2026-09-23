"""Pinned Socketify ctypes experiment; not a supported listener-adoption API."""
import ctypes
import hashlib
import os
from pathlib import Path
import signal
import socket
import sys

from socketify import App
from socketify.native import ffi, library_path


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


if sys.stdin.readline() != "ordinary stdin\n":
    raise RuntimeError("Ordinary stdin is not usable")
handle = int(os.environ["OOTH_LISTEN_HANDLE"])
app = App()
app.get("/", lambda response, request: response.end(f"{os.getpid()}\n"))
temporary = adopt_listener(app, handle)
print(f"adopted inherited={handle} closed_temporary={temporary}", file=sys.stderr, flush=True)

# Only tests normal listener closure; there is no request-draining protocol yet.
def stop(signum, frame):
    app.close()


signal.signal(signal.SIGTERM, stop)
if hasattr(signal, "SIGBREAK"):
    signal.signal(signal.SIGBREAK, stop)
print("ready", flush=True)
app.run()
app.loop.uv_loop.run_nowait()
print("closed", flush=True)
