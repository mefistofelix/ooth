"""HTTPServer with its own SO_REUSEPORT listener, no inherited socket."""
from http.server import BaseHTTPRequestHandler, HTTPServer
import os
import socket
import sys
import threading
import time
from urllib.parse import urlparse


def event(kind, **fields):
    values = {"v": 1, "event": kind, "ts": time.time_ns(), **fields}
    print(" ".join(f"{key}={value}" for key, value in values.items()), flush=True)


sequence = 0


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        global sequence
        sequence += 1
        request_id = sequence
        started = time.monotonic_ns()
        event("start", id=request_id)
        try:
            time.sleep(min(2000, max(0, int(self.path.strip("/") or "0"))) / 1000)
            body = f"worker={os.getpid()} request={request_id} protocol=http1\n".encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
        finally:
            event("end", id=request_id, duration_ns=time.monotonic_ns() - started)

    def log_message(self, format, *args):
        pass


address = urlparse(sys.argv[1])
# HTTPServer's constructor normally binds immediately. Set reuseport first.
server = HTTPServer((address.hostname, address.port), Handler, bind_and_activate=False)
server.socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
server.server_bind()
server.server_activate()


def control():
    for line in sys.stdin:
        if line.startswith("v=1 event=stop ts="):
            server.shutdown()
            return


threading.Thread(target=control, daemon=True).start()
event("ready")
server.serve_forever(poll_interval=0.05)
server.server_close()
