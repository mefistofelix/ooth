# Tests

Run commands from the repository root, after preparing the patched Go compiler
with `bash ./build.sh`:

```powershell
$env:CGO_ENABLED = '0'
./build/windows-amd64/go/bin/go.exe run ./test/run.go test -v -count=1 -timeout 180s ./...
./build/windows-amd64/go/bin/go.exe run ./test/run.go vet ./...
```

```sh
CGO_ENABLED=0 ./build/linux-amd64/go/bin/go run ./test/run.go test -v -count=1 -timeout 180s ./...
CGO_ENABLED=0 ./build/linux-amd64/go/bin/go run ./test/run.go vet ./...
```

`run.go` uses Go's `-overlay` option to include the `test/*_test.go` files in
the package under `src/`. Private implementation tests remain possible without
copying source files or exporting internals. Go selects the platform-specific
test files normally. Tests run with `src/` as their working directory; relative
Go flags such as `-o` are also relative to `src/`. The temporary overlay file is
removed after each invocation. The build script uses the same runner.

The [integration fixtures](integration/README.md) contain workers, proxy
configurations, runtime preparation scripts, research probes and saved aggregate
results. Each suite documents its optional `OOTH_TEST_*` variables; executable
and artifact paths must be absolute. The default build skips suites whose
runtimes have not been selected.

Downloaded runtimes, reference source checkouts and generated logs/configurations
belong in the root `tmp/` directory, which Git ignores. They can be regenerated
using the preparation instructions. The root `build/` directory is reserved for
the Go toolchains; `bin/` holds ooth release binaries.
