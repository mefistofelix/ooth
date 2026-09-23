# Listener pressure experiments

These opt-in experiments measure what ooth can infer from its listener notifications and worker events. They run the actual supervisor, inherited sockets and independent processes. They do not alter the production scaling policy. `main.go` adds one debug event, `listener readable`, with the timestamp captured immediately after `EpollWait` returns, before delivery to the manager. This is an observation time, not the kernel's connection-arrival timestamp.

See [RESULTS.md](RESULTS.md) for measured counterexamples, cross-platform comparisons and the resulting scaling recommendation. Every workload here uses loopback TCP; Unix-socket functionality is covered separately by the normal integration suite.

`pressure_test.go` contains the driver and a Go worker. The Python and Node fixtures here use HTTP, and the PHP fixture runs under unmodified PHP CGI using FastCGI. Node uses HTTP/1.1 directly and h2c through the proxies. Its Windows handle adoption remains the experimental built-in FFI/private binding documented in the parent probe README. Windows PHP uses the existing stdin-only launcher; Linux launches PHP CGI directly.

`TestPressureProxies` starts Caddy and Nginx through ooth's init mode, reusing the committed proxy configuration templates. Python upstream connections close after each request. Node h2c sessions stay open and allow multiplexing: there is deliberately no GOAWAY rotation to distribute load artificially. PHP uses FastCGI. The pressure suite increases Nginx's connection allowance from 128 to 1024 so the proxy's descriptor cap does not become the experiment's bottleneck.

## Workloads

- Open-loop arrivals: 10/s and 70/s for two seconds, independently of earlier responses; 1, 2 and 4 prestarted workers. No closed-loop client throttle hides growing queues.
- Fixed 40 ms service delay in the Go fixtures; repeatable 5–75 ms delays in Python, Node and PHP, calculated from request ID. This varies service time while preserving the same sequence across runs.
- Serial Go/Python/PHP execution; Go accepting ahead of a single execution slot; Go/Node asynchronous sleeps; Node deliberately blocking its event loop; a serial worker receiving pipelined requests on one persistent connection.
- Additional Go cases: a burst of 80 connections, 200/s with a 1 ms delay, and the current autoscaler from 1 to 4 workers with `concurrency` 1 or 4 and `scale_delay: 100ms`.
- PHP is intentionally ordinary output/no handshake. Its worker count is configured for comparison; it does not acquire request-based autoscaling in these tests.

All requests must receive a valid response. Measurements are reported, not asserted against machine-dependent performance thresholds. Repetitions are sequential; run Windows and Linux separately to avoid making the other test run a source of pressure.

## Recorded data and limits

Each case keeps its generated YAML, proxy configuration, `events.csv`, `requests.csv` and `scenario.csv` in `OOTH_TEST_PRESSURE`. The client records scheduled arrival, actual dispatch, completion of its connection attempt and receipt of its response. Test response bodies add the worker PID, request-start timestamp and accept observation where available. This does **not** add `accept` to ooth's ready/start/end protocol or introduce a production control channel.

- Go and Python record the return of accept; Node records the runtime's connection/session callback, with millisecond wall-clock resolution. PHP cannot expose its native accept here, so that field is zero, meaning unavailable.
- For direct fresh connections, connect-to-accept is an approximation. A server may accept before the client returns from connect; negative differences are clamped to zero. This is not a kernel-level queue timestamp.
- Dispatch-to-request-start includes network/runtime/proxy overhead as well as any queue wait. Compare it with the corresponding low-load baseline. Through a proxy, the client connection timestamp refers to the frontend, not the upstream socket.
- Node h2c responses retain the timestamp of their shared session. New streams do not constitute new accepts. Existing sessions can pin traffic to one PID even when more workers are available.
- Event-to-next-accept is a **heuristic pairing**, not connection identity. A worker can accept before the notification is observed; pairing that event with a later accept can exaggerate the delay. Multiple notifications can refer to the same future accept. Unmatched events remain missing samples, not zero latency.
- The analyzer also reports distance to the nearest accept before or after each observation. This helps expose stale-event pairings, but measures proximity to accept activity, **not** a connection's waiting time. It cannot identify which accept consumed the connection that made the listener readable.
- `readable_bins_pct` counts 50 ms arrival-window bins containing a notification. The longest run joins observations no more than 75 ms apart. Neither proves the queue remained nonempty between observations; arrivals can refill it between rearms. `queue_alarm_ms` is an offline candidate (such a run lasting 100 ms), not a new production setting.
- The listener uses ooth's existing 25 ms manager tick and delayed ONESHOT rearm; this limits event frequency. A low event count cannot by itself rule out queueing inside a worker or proxy.
- These loopback synthetic workloads test observability and counterexamples. They do not establish a universal optimal scaling threshold, production throughput, or CPU/memory-pressure behavior.

The CSV analyzer reports the proposed notification-to-next-accept pairing, notification density/run length, serving PIDs, request-event counts and latency percentiles separately. It does not combine them into an unexplained pressure score. Request-start/end counts should equal the number of successful requests for the protocol workers and remain zero for stock PHP. This makes missing telemetry distinguishable from low activity.

## Reproduce

Prepare the dedicated Go toolchains using `build.sh` and the existing runtimes following `../proxy/README.md`. No new runtime or application dependency is installed by these tests. Set `OOTH_TEST_PYTHON`, `OOTH_TEST_NODE`, `OOTH_TEST_PHP_CGI`, `OOTH_TEST_CADDY`, and `OOTH_TEST_NGINX` to native absolute paths. On Windows also set `OOTH_TEST_PHP_LAUNCHER`.

Windows, after setting those runtime paths:

```powershell
$env:CGO_ENABLED = '0'
$env:OOTH_TEST_PRESSURE = "$PWD/build/pressure-windows"
./build/windows-amd64/go/bin/go.exe test -run '^TestPressure(Proxies)?$' -count=2 -timeout 12m .
python tools/probes/pressure/analyze.py build/pressure-windows --platform windows --output build/pressure-windows.csv
```

Linux, after setting the Linux runtime paths (do not inherit Windows paths):

```sh
export CGO_ENABLED=0 OOTH_TEST_PRESSURE="$PWD/build/pressure-linux"
./build/linux-amd64/go/bin/go test -run '^TestPressure(Proxies)?$' -count=2 -timeout 12m .
python3 tools/probes/pressure/analyze.py build/pressure-linux --platform linux --output build/pressure-linux.csv
```

Use a writable delegated cgroup for whole-family supervision on Linux, as described in `../proxy/README.md` and implemented by `../with-cgroup.sh`. The ordinary build skips these opt-in experiments. No GitHub workflow is dispatched. Raw local artifacts belong under ignored `build/`; portable aggregate CSVs and the interpretation belong here.
