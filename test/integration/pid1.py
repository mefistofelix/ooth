import json
import os
import pathlib
import signal
import subprocess
import tempfile
import time

binary = pathlib.Path('bin/ooth-linux-amd64').resolve()
with tempfile.TemporaryDirectory(prefix='ooth-pid1-') as directory:
    root = pathlib.Path(directory)
    (root / 'ooth.yaml').write_text("watch: ['app/app.yaml']\n")
    (root / 'app').mkdir()
    worker = root / 'worker.py'
    worker.write_text('''import os, time
child = os.fork()
if child == 0:
    orphan = os.fork()
    if orphan == 0:
        time.sleep(0.2)
        os._exit(0)
    os._exit(0)
os.waitpid(child, 0)
while True: time.sleep(1)
''')
    (root / 'app/app.yaml').write_text('name: orphan-maker\nstartup: true\ncommand: [/usr/bin/python3, ' + json.dumps(str(worker)) + ']\nstop_timeout: 1s\n')
    parent = subprocess.Popen(['unshare', '-Urpf', '--mount-proc', str(binary), '-config', str(root / 'ooth.yaml')], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        time.sleep(0.4)
        children = pathlib.Path(f'/proc/{parent.pid}/task/{parent.pid}/children').read_text().split()
        assert len(children) == 1, children
        pid = int(children[0])
        status = pathlib.Path(f'/proc/{pid}/status').read_text()
        assert any(line.split()[-1] == '1' for line in status.splitlines() if line.startswith('NSpid:')), status
        time.sleep(2)
        zombies = []
        for entry in pathlib.Path(f'/proc/{pid}/root/proc').iterdir():
            if entry.name.isdigit():
                try:
                    state = (entry / 'stat').read_text().rsplit(') ', 1)[1].split()[0]
                    if state == 'Z': zombies.append(entry.name)
                except FileNotFoundError:
                    pass
        assert not zombies, zombies
        os.kill(pid, signal.SIGTERM)
        assert parent.wait(timeout=4) == 0
        print('PASS: actual namespace PID 1 reaped an adopted zombie and shut down on SIGTERM')
    finally:
        if parent.poll() is None:
            for child in pathlib.Path(f'/proc/{parent.pid}/task/{parent.pid}/children').read_text().split():
                os.kill(int(child), signal.SIGKILL)
            parent.wait(timeout=4)
        print(parent.stderr.read().decode())
