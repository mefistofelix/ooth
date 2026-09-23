"""Serial pressure fixture, Python stdlib. Test timestamps travel in responses."""
import os
import signal
import socket
import sys
import time
from urllib.parse import parse_qs, urlsplit

stopping = False


def stop(*_):
    global stopping
    stopping = True


def event(kind, **fields):
    values = {"v": 1, "event": kind, "ts": time.time_ns(), **fields}
    print(" ".join(f"{key}={value}" for key, value in values.items()), flush=True)


signal.signal(signal.SIGINT, stop)
if os.name == "nt":
    import ctypes
    signal.signal(signal.SIGBREAK, stop)
    get_handle = ctypes.windll.kernel32.GetStdHandle
    get_handle.restype = ctypes.c_size_t
    descriptor = get_handle(-10)
else:
    signal.signal(signal.SIGTERM, stop)
    descriptor = 0

listener = socket.socket(fileno=descriptor)
listener.settimeout(0.2)
event("ready")
while not stopping:
    try:
        connection, _ = listener.accept()
    except (TimeoutError, BlockingIOError):
        continue
    accepted = time.time_ns()
    with connection:
        connection.settimeout(10)
        with connection.makefile("rb") as reader:
            line = reader.readline().decode()
            if not line:
                continue
            query = parse_qs(urlsplit(line.split()[1]).query)
            while reader.readline() not in (b"\r\n", b"", b"\n"):
                pass
            ident = query["id"][0]
            started = time.time_ns()
            event("start", id=ident)
            time.sleep(int(query["delay"][0]) / 1000)
            event("end", id=ident, duration_ns=time.time_ns() - started)
            body = f"{ident} {os.getpid()} {accepted} {started}\n".encode()
            header = f"HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: {len(body)}\r\n\r\n".encode()
            connection.sendall(header + body)
listener.close()
