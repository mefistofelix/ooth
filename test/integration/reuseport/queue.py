"""Linux diagnostic: a new reuseport listener cannot accept an older queue."""
import json
import os
import select
import socket
import subprocess
import sys


def listen(port):
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
    listener.bind(("127.0.0.1", port))
    listener.listen()
    return listener


if len(sys.argv) > 1:
    with listen(int(sys.argv[1])) as listener:
        if len(sys.argv) > 2:
            print("ready", flush=True)
            sys.stdin.readline()
            count = 0
            while select.select([listener], [], [], 0)[0]:
                accepted, _ = listener.accept()
                accepted.close()
                count += 1
            print(count, flush=True)
            sys.exit(0)
        ready = bool(select.select([listener], [], [], 0.5)[0])
        print(json.dumps({"pid": os.getpid(), "address": listener.getsockname(), "readable": ready}))
    sys.exit(0)

if not sys.platform.startswith("linux"):
    sys.exit("This queue diagnostic requires Linux SO_REUSEPORT")

with listen(0) as first:
    address = first.getsockname()
    with socket.create_connection(address) as client:
        assert select.select([first], [], [], 1)[0], "first listener did not become readable"
        # The second process starts only after the original connection is queued.
        result = subprocess.run([sys.executable, __file__, str(address[1])],
                                check=True, capture_output=True, text=True, timeout=5)
        second = json.loads(result.stdout)
        assert second["address"] == list(address), second
        assert not second["readable"], "new listener unexpectedly inherited queued connection"
        assert select.select([first], [], [], 0)[0], "original listener lost its queue"
        # Diagnostic only: identify which queue contains the original connection.
        # This is not ooth's activation code and forwards no application traffic.
        accepted, peer = first.accept()
        with accepted:
            assert peer == client.getsockname()
        print("PASS: first listener readable before spawn; second process binds/listens on")
        print("the same port but stays unreadable; original connection remains in first queue.")

    second = subprocess.Popen([sys.executable, __file__, str(address[1]), "count"],
                              stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    clients = []
    try:
        assert second.stdout.readline() == "ready\n"
        for _ in range(32):
            clients.append(socket.create_connection(address, timeout=2))
        output, _ = second.communicate("count\n", timeout=5)
        assert second.returncode == 0
        worker_count = int(output)
        first_count = 0
        while select.select([first], [], [], 0)[0]:
            accepted, _ = first.accept()
            accepted.close()
            first_count += 1
        assert first_count + worker_count == len(clients)
        assert first_count > 0 and worker_count > 0
        print(f"PASS: new clients reach both separate queues: original={first_count}, worker={worker_count}.")
    finally:
        for client in clients:
            client.close()
        if second.poll() is None:
            second.kill()
            second.wait()
