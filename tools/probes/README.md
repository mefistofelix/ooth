# Manual platform probes

Run these from the repository root after `bash ./build.sh`. They use only the dedicated patched compiler and OS/interpreter facilities. The Go files have `ignore` build tags because they are standalone probes, not application packages.

Linux (requires Python 3, `unshare`, user namespaces and procfs):

```sh
python3 tools/probes/pid1.py
CGO_ENABLED=1 ./build/linux-amd64/go/bin/go test -race -timeout 60s .
```

The first probe creates a real PID namespace with ooth as PID 1, starts a process that creates an orphan, checks that no zombie remains and stops ooth with SIGTERM. It cleans up the namespace on failure.

Windows, from PowerShell (the GUI probe uses the installed .NET Framework C# compiler):

```powershell
& "$env:WINDIR/Microsoft.NET/Framework64/v4.0.30319/csc.exe" '/nologo' '/target:winexe' '/reference:System.Windows.Forms.dll' '/reference:System.Drawing.dll' "/out:$PWD\build\GuiProbe.exe" "$PWD\tools\probes\GuiProbe.cs"
& ./build/windows-amd64/go/bin/go.exe run tools/probes/probe_gui.go
& ./build/windows-amd64/go/bin/go.exe test -c -ldflags '-H=windowsgui' -o bin/ooth-no-console.test.exe .
& ./build/windows-amd64/go/bin/go.exe run tools/probes/probe_detached.go
```

The GUI is transparent and minimized. Its FormClosing handler records receipt of WM_CLOSE before exit. The detached probe runs both graceful and forceful worker tests with no inherited standard handles and no console. These probes do not install services or alter the user's global Go installation.
