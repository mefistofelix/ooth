"""Download pinned test runtimes into build/, without installing globally."""
import hashlib
import json
import os
from pathlib import Path
import sys
import urllib.request
import zipfile

platform = sys.argv[1] if len(sys.argv) > 1 else ("windows" if os.name == "nt" else "linux")
if platform not in ("windows", "linux"):
    raise SystemExit("Usage: python prepare.py [windows|linux] (amd64 only)")
root = Path(__file__).resolve().parents[3] / "build" / "runtime-js"
root.mkdir(parents=True, exist_ok=True)
manifest = json.loads(Path(__file__).with_name("archives.json").read_text())
for asset in manifest:
    if ("windows" in asset["name"]) != (platform == "windows"):
        continue
    archive = root / asset["name"]
    if not archive.exists():
        urllib.request.urlretrieve(asset["browser_download_url"], archive)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    if "sha256:" + digest != asset["digest"]:
        raise SystemExit(f"Checksum mismatch: {archive}")
    target = root / archive.stem
    with zipfile.ZipFile(archive) as bundle:
        bundle.extractall(target)
    executable = next(target.rglob("bun.exe" if platform == "windows" else "bun"), None)
    if executable is None:
        executable = target / ("deno.exe" if platform == "windows" else "deno")
    if platform == "linux":
        executable.chmod(0o755)
    print(executable)
