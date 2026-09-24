# Caddy / Nginx end-to-end suite

`TestProxyStack` starts the **webserver itself through ooth's init mode** (`startup: true`). The three upstream listeners already belong to ooth, but no upstream worker starts until a request arrives. All traffic goes directly between webserver and workers; ooth only observes readiness and stdout events. Every listener in this suite is TCP on loopback; separate Go-worker tests cover Unix-socket ownership, ACLs and inheritance on both platforms.

| Upstream | Protocol | Assertions |
| --- | --- | --- |
| `examples/worker.py` | HTTP/1.1 | Cold activation, two worker PIDs, balanced start/end events, idle pool at zero, reactivation |
| `node_worker.cjs` with `TEST_NODE_H2C=1` | HTTP/2 cleartext (h2c) | Same lifecycle, with an explicit h2c response marker |
| Unmodified `php-cgi` | FastCGI | Cold response, no handshake/events, one process retained beyond request and idle timeouts |

`Caddyfile.tmpl`, `nginx.conf.tmpl`, `ooth.yaml`, `webserver.yaml.tmpl` and `worker.yaml.tmpl` are the actual configuration sources used by the suite. It renders them with temporary directories, free local ports and runtime paths. `worker.php` is the PHP response fixture, not a telemetry adapter. Set `OOTH_TEST_ARTIFACTS` to retain every rendered configuration and `ooth.log` under that directory; otherwise Go removes the temporary files after the test. An external runtime is used only when its explicit test variable is set. The normal build does not download these runtimes or change GitHub CI.

The suite uncovered and now covers an idle-retirement bug: draining socket workers must retain their dependencies, but must not cause ooth to spawn replacements solely because they are still draining. The normal `TestActivationLifecycle` also asserts that the pool stays at zero before another request arrives.

## Tested versions and protocol details

Local Linux and Windows checks used Caddy 2.11.4, Nginx 1.30.5, Node 26.10.0, the local Python interpreters, and PHP 8.3.6 on Ubuntu / 8.4.26 on Windows. The preparation scripts pin downloads and verify SHA-256; the Ubuntu PHP package is checked against the configured signed apt repository metadata. No global runtime or package installation is performed. Linux preparation requires Ubuntu 24.04 amd64, `curl`, `tar`, a C compiler, `make`, `apt-get` and `dpkg-deb`, plus the runtime libraries required by that Ubuntu PHP package. Windows PHP requires the corresponding Visual C++ runtime. Other architectures can use externally supplied compatible binaries.

Caddy documents its [h2c upstream transport](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#the-http-transport). Nginx requires a version supporting [`proxy_http_version 2`](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_http_version), added in 1.29.4. These tests use HTTP/1.1 from client to proxy and h2c from proxy to Node; TLS and public listeners are unnecessary.

HTTP/2 streams on an existing connection stay with the worker that accepted it. Scaling processes cannot move those streams. The test Node fixture limits concurrent streams per session and sends GOAWAY after each response, so subsequent new connections can exercise new workers. The test explicitly distinguishes pool growth from distribution of requests already accepted before that growth. This is a lifecycle fixture, not a benchmark or a production connection-pooling recommendation. On Windows, Node still uses the documented experimental native-handle/private-binding fixture described in the parent probe README.

## Windows PHP compatibility

Stock Windows PHP detects inherited FastCGI only when stdin is valid **and both stdout and stderr are `INVALID_HANDLE_VALUE`**; see [`fcgi_init` in PHP 8.4.26](https://github.com/php/php-src/blob/php-8.4.26/main/fastcgi.c#L501). With ordinary output handles, the original binary enters CGI mode and exits without accepting the pending FastCGI connection. `TestStockPHPWindowsInheritedDetection` preserves that negative compatibility probe.

`../php_worker_windows.go` is a small standalone launcher, compiled without CGO. It starts the unmodified PHP binary with that stdio convention, inherits the existing listener, console group and Job, and waits for PHP's exit. It never accepts, proxies or copies traffic and adds no toolchain patch. The ooth process tracked in this case is the launcher; PHP remains its owned descendant. The Windows positive suite uses this launcher explicitly through `OOTH_TEST_PHP_LAUNCHER`. Linux starts php-cgi directly. PHP emits no ooth handshake, so it has no request-based growth/shrinking/watchdog; these tests do not claim PHP drains active requests gracefully on Windows. The usual whole-family forceful timeout remains available.

## Reproduce on Windows

Run the regular build first to prepare the dedicated Go compiler. Use the Python/Node installations from the earlier worker probes, or equivalent compatible binaries:

```powershell
./tools/probes/proxy/prepare-windows.ps1
$env:CGO_ENABLED = '0'
$env:OOTH_TEST_PYTHON = 'C:/path/to/python.exe'
$env:OOTH_TEST_NODE = 'C:/path/to/node.exe'
$env:OOTH_TEST_CADDY = "$PWD/build/runtime-proxy/caddy-windows/caddy.exe"
$env:OOTH_TEST_NGINX = "$PWD/build/runtime-proxy/nginx-1.30.5/nginx.exe"
$env:OOTH_TEST_PHP_CGI = "$PWD/build/runtime-proxy/php-windows/php-cgi.exe"
$env:OOTH_TEST_PHP_LAUNCHER = "$PWD/build/runtime-proxy/php-worker.exe"
$env:OOTH_TEST_ARTIFACTS = "$PWD/build/proxy-results-windows"
./build/windows-amd64/go/bin/go.exe test -run 'TestProxyStack|TestStockPHPWindowsInheritedDetection' -v -timeout 90s .
```

## Reproduce on Linux

```sh
bash tools/probes/proxy/prepare-linux.sh
export CGO_ENABLED=0
export OOTH_TEST_PYTHON=/usr/bin/python3
export OOTH_TEST_NODE=/absolute/path/to/node
export OOTH_TEST_CADDY="$PWD/build/runtime-proxy/caddy-linux/caddy"
export OOTH_TEST_NGINX="$PWD/build/runtime-proxy/nginx-linux/objs/nginx"
export OOTH_TEST_PHP_CGI="$PWD/build/runtime-proxy/php-linux/usr/bin/php-cgi8.3"
export OOTH_TEST_ARTIFACTS="$PWD/build/proxy-results-linux"
./build/linux-amd64/go/bin/go test -run TestProxyStack -v -timeout 90s .
```

For full descendant ownership, run from a writable delegated cgroup. The local setup helper runs the tests as an ordinary user inside a temporary delegation; only setup/cleanup use root:

```sh
sudo --preserve-env=OOTH_TEST_PYTHON,OOTH_TEST_NODE,OOTH_TEST_CADDY,OOTH_TEST_NGINX,OOTH_TEST_PHP_CGI,OOTH_TEST_ARTIFACTS \
  bash tools/probes/with-cgroup.sh "$USER" env CGO_ENABLED=0 \
  ./build/linux-amd64/go/bin/go test -run TestProxyStack -v -timeout 90s .
```

The helper's private subtree is terminated and removed on exit. It never modifies an existing service's cgroup. Without delegation, the suite can still exercise the documented direct-child fallback, with its stderr warning.

## PHP-CGI listener selection

The worker template explicitly sets `socket_handoff: stdin` for stock PHP-CGI;
Python/Node use the new default extra-handle/environment convention and stdin
stop after their ready handshake. PHP has no handshake and receives the existing
OS graceful notification, followed by forceful timeout if needed.

Audited the standard [CGI entry point](https://github.com/php/php-src/blob/PHP-8.5/sapi/cgi/cgi_main.c)
and [FastCGI implementation](https://github.com/php/php-src/blob/PHP-8.5/main/fastcgi.c),
and checked the actual Windows binary's `-h` output. `-b` / `--bindpath` opens a
new listener from an address/port; without it the inherited listener is fd 0.
No environment selector for an arbitrary inherited listener was found in this
CGI path. `FCGI_WEB_SERVER_ADDRS` filters clients; `_FCGI_MUTEX_` and
`_FCGI_SHUTDOWN_EVENT_` are Windows synchronization/control handles.
`FPM_SOCKETS` belongs to [PHP-FPM](https://github.com/php/php-src/blob/PHP-8.5/sapi/fpm/fpm/fpm_sockets.c),
which is not the Windows CGI binary. The similarly named
[`FCGI_LISTENSOCK_FILENO`](https://github.com/FastCGI-Archives/fcgi2/blob/master/include/fastcgi.h)
is a C constant with value 0, not an environment variable.
