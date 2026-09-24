# Pressure measurements and scaling implications

These measurements distinguish **work waiting to start** from long-running work. A 40 ms asynchronous operation need not be overloaded; a request waiting several seconds before its handler starts is a different condition. The client schedule is independent of earlier replies, so growing queues are visible.

The host was an Intel i7-11800H (16 logical CPUs), Windows 11 10.0.26200 and Ubuntu 24.04 under WSL2 kernel 6.6.87.2. Windows and Linux runs were sequential, using the dedicated Go 1.27.1 toolchains with CGO disabled. Python was 3.14.3 on Windows / 3.12.3 on Linux; Node 26.10.0, Caddy 2.11.4 and Nginx 1.30.5 on both; PHP CGI 8.4.26 on Windows / 8.3.6 on Linux. This is one development machine, not a production capacity benchmark or validation on every kernel/runtime release.

On 2026-09-23/24, all **244 case executions passed**: 61 scenarios, twice on each OS, with **26,560 successful requests**. All protocol-worker request-start/end counts matched their request counts; stock PHP emitted neither. Aggregate rows retain both repetitions rather than hiding variation in an average. Raw traces and generated configurations are retained locally in `tmp/pressure-smoke` (the completed Windows matrix) and `tmp/pressure-linux`; initial smoke attempts are excluded by selecting the latest two complete runs per scenario.

## Observations

Selected cases at 70 requests/s with variable 5–75 ms delays; ranges span the two repetitions:

| Workload | Workers | Windows p95 wait, ms | Linux p95 wait, ms | Windows events/s | Linux events/s |
| --- | ---: | ---: | ---: | ---: | ---: |
| Caddy → Python | 1 | 3635–3659 | 3581–3584 | 20.5 | 20–20.5 |
| Caddy → Python | 4 | 1.7–2.0 | 1.7–1.8 | 20 | 16.5–17.5 |
| Caddy → PHP CGI | 4 | 8.5–11.4 | 3.4–4.0 | 5.5–8 | 19.5–20 |
| Caddy → blocking Node/h2c | 4 | 3510–3511 | 3538–3539 | 0 | 0 |
| Nginx → blocking Node/h2c | 4 | 1667–1723 | 1.0–1.1 | 0 | 12.5–14 |

For the last two rows, Caddy used one serving PID on both OSes; Nginx used two on Windows and four on Linux. They all had four prestarted workers.

The complete aggregate measurements are in `windows.csv` and `linux.csv`; columns and reproduction commands are explained in [README.md](README.md). Times above are dispatch-to-request-start p95, including proxy/runtime overhead. They are not isolated kernel queue delays. Compare against each stack's low-load baseline.

### Notification count and repeated notifications are insufficient

The same well-provisioned workload can produce frequent notifications on Linux and almost none on Windows. Windows Python also produces repeated observations with four workers and negligible waiting. An event says a connection was available to accept when observed; it does not establish how long it waited. The delayed ONESHOT rearm makes event frequency depend on the observation schedule as well as arrival rate and accept implementation.

A chain of notifications less than 75 ms apart for 100 ms is therefore not proof that the queue remained nonempty for 100 ms. These experiments include both false positives (healthy workers) and false negatives (queued work inside a worker or an existing connection). The offline `queue_alarm_ms` column demonstrates that candidate; it is not a production setting.

### Notification-to-next-accept can be helpful, but has stale-event errors

For Python receiving fresh connections, the heuristic separated overloaded serial workers from a sufficiently large pool in these samples. However, a low-load Go worker on Linux also produced roughly 100 ms event-to-next-accept despite approximately 1 ms dispatch-to-start. The actual accept preceded the observation, so the heuristic paired it with the next client's accept, one arrival interval later.

The distance to the nearest accept before **or** after the observation exposes that false pairing. It measures proximity to accept progress, not queue age. Neither pairing identifies the connection or counts pending accepts. In these tests accept observations are supplied in response bodies for offline analysis; the current ooth stdout protocol does not report them, and stock PHP cannot provide them here.

### Accepting ahead hides application queues

The Go greedy fixture accepts connections promptly but permits only one request to execute at a time. Its application queue grows while connect-to-accept remains small. An asynchronous fixture can show similarly prompt accepts while executing overlapping sleeps without a queue. The accept metric alone cannot distinguish those execution models.

### h2c pooling changes whether more workers help

Caddy keeps the test's h2c traffic on one session/worker. An asynchronous Node handler serves it without substantial queueing. Replacing the asynchronous sleep with a blocking event-loop delay creates a long wait, including when four workers are prestarted: the existing session remains with its accepting worker. There are almost no listener observations in either case.

Nginx behaves differently in these runs, and its distribution also varies by OS; the per-case serving PID count records that distinction. Worker count is not a guarantee of upstream distribution. The suite deliberately keeps h2c sessions open instead of forcing GOAWAY to make scaling appear effective.

### The existing policy helps serial workers but does not infer capacity

With the synthetic serial Go worker, current request-based growth from one to four workers sharply reduces waiting compared with a fixed single worker. With asynchronous sleeps, `concurrency: 1` grows a pool even though one process already handles the load with negligible waiting; `concurrency: 4` avoids that growth in this workload. With the single persistent connection, it starts a second process that receives no requests and cannot relieve the original connection's queue.

These are different pressure conditions with overlapping observable signals. CPU/memory samples would not recover the missing request/connection ownership information. They can be resource limits, but are not substitutes for measuring queueing.

## Recommendation

There is **no universal pressure threshold justified by the current listener and start/end data**. In particular, do not replace the existing policy with “notification received → add worker”, a raw notifications/second threshold, or an unqualified event-to-next-accept timer.

For a future policy driven by actual waiting:

1. Prefer a request queue-delay signal measured where work really enters the queue, before an execution slot is obtained. For multiplexed protocols this must be per request/stream, not per connection. A callback that only runs after an event-loop stall cannot retroactively measure that stall; it needs an earlier observation point or complementary loop-lag telemetry.
2. Treat listener/accept observations as supporting evidence for fresh-connection workloads. Missing samples stay unknown. Stock PHP has no request telemetry, so a generic policy based only on its listener would remain a documented heuristic with these blind spots.
3. Use a sustained delay budget over a window, rather than reacting to one slow request or one notification. Grow one worker, respect readiness and a stabilization interval, and retain `max_workers` as a resource bound. The delay/window values need workload-specific calibration; these two-second experiments do not select production defaults.
4. Verify that new workers receive work and relieve measured waiting. If requests remain pinned to existing connections, adding more processes is not a fix; upstream connection/stream allocation must permit redistribution of future work.

The production scaling policy is unchanged. This work provides the reproducible evidence needed to choose the next protocol/policy change without confusing callback timing, request duration and queue waiting.
