"""Minimal HTTP worker illustrating the ooth contract; Python stdlib only."""
import os
import signal
import socket
import sys
import time
import threading

stopping = False


def stop(signum, frame):
    global stopping
    stopping = True


def event(kind, **fields):
    values = {"v": 1, "event": kind, "ts": time.time_ns(), **fields}
    print(" ".join(f"{key}={value}" for key, value in values.items()), flush=True)


signal.signal(signal.SIGINT, stop)
if os.name == "nt":
    import ctypes

    signal.signal(signal.SIGBREAK, stop)
    get_std_handle = ctypes.windll.kernel32.GetStdHandle
    get_std_handle.restype = ctypes.c_size_t
    descriptor = get_std_handle(-10)  # STD_INPUT_HANDLE is a Winsock socket.
else:
    signal.signal(signal.SIGTERM, stop)
    descriptor = 0

if "OOTH_LISTEN_HANDLE" in os.environ:
    descriptor = int(os.environ["OOTH_LISTEN_HANDLE"])

    def control():
        for line in sys.stdin:
            fields = dict(field.split("=", 1) for field in line.split())
            if fields.get("v") == "1" and fields.get("event") == "stop":
                stop(None, None)
                return

    threading.Thread(target=control, daemon=True).start()

listener = socket.socket(fileno=descriptor)
listener.settimeout(0.2)
event("ready")
sequence = 0
while not stopping:
    try:
        connection, address = listener.accept()
    except (TimeoutError, BlockingIOError):
        continue
    sequence += 1
    started = time.monotonic_ns()
    event("start", id=sequence)
    try:
        with connection:
            connection.settimeout(10)
            request = b""
            while b"\r\n\r\n" not in request and len(request) < 8192:
                data = connection.recv(1024)
                if not data:
                    break
                request += data

            # A numeric URL such as /600 lets the integration suite hold work
            # for 600 ms while checking scaling; ordinary paths have no delay.
            try:
                delay_ms = int(request.split(b" ", 2)[1].strip(b"/"))
            except (ValueError, IndexError):
                delay_ms = 0
            time.sleep(max(0, min(delay_ms, 2000)) / 1000)
            body = f"worker={os.getpid()} request={sequence}\n".encode()
            response = b"HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: "
            connection.sendall(response + str(len(body)).encode() + b"\r\n\r\n" + body)
    except OSError as error:
        print(error, file=sys.stderr)
    finally:
        event("end", id=sequence, duration_ns=time.monotonic_ns() - started)
listener.close()
