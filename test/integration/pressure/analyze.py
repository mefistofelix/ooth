"""Summarize TestPressure CSVs, stdlib only; no timing pass/fail thresholds."""
import argparse
import bisect
import csv
import math
from pathlib import Path


def read(path):
    with path.open(newline="", encoding="utf-8") as stream:
        return list(csv.DictReader(stream))


def percentile(values, fraction):
    if not values:
        return ""
    values = sorted(values)
    return round(values[min(len(values) - 1, math.ceil(len(values) * fraction) - 1)], 3)


def summarize(directory, platform):
    scenario = read(directory / "scenario.csv")[0]
    requests = read(directory / "requests.csv")
    events = read(directory / "events.csv")
    valid = [row for row in requests if not row["error"]]
    start = int(scenario["start_ns"])
    end = int(scenario["load_end_ns"])
    # Burst duration is the response-drain interval, paced cases use their
    # scheduled arrival interval. Never divide events by response throughput.
    if scenario["burst"] == "true":
        end = max(int(row["done_ns"]) for row in valid)
    notices = sorted(int(row["time_ns"]) for row in events
                     if row["event"] == "listener readable" and start <= int(row["time_ns"]) < end)
    accepts = sorted({(int(row["accepted_ns"]), row["pid"]) for row in valid if int(row["accepted_ns"]) > 0})
    accept_times = [timestamp for timestamp, _ in accepts]
    future_accept = []
    nearest_accept = []
    for notice in notices:
        index = bisect.bisect_left(accept_times, notice)
        if index < len(accept_times):
            future_accept.append((accept_times[index] - notice) / 1e6)
        neighbors = accept_times[max(0, index - 1):index + 1]
        if neighbors:
            nearest_accept.append(min(abs(timestamp - notice) for timestamp in neighbors) / 1e6)
    longest = 0
    first_alarm = ""
    run_start = previous = None
    for notice in notices:
        if previous is None or notice - previous > 75_000_000:
            run_start = notice
        elapsed = notice - run_start
        longest = max(longest, elapsed)
        if elapsed >= 100_000_000 and first_alarm == "":
            first_alarm = round((notice - start) / 1e6, 3)
        previous = notice
    occupied_bins = {int((notice - start) // 50_000_000) for notice in notices}
    waits = [max(0, int(row["started_ns"]) - int(row["dispatched_ns"])) / 1e6 for row in valid]
    accept_waits = []
    if not scenario["proxy"] and scenario["persistent"] != "true":
        accept_waits = [max(0, int(row["accepted_ns"]) - int(row["connected_ns"])) / 1e6
                        for row in valid if int(row["accepted_ns"]) > 0]
    result = {
        "platform": platform,
        "case": scenario["name"],
        "requests": len(requests),
        "errors": len(requests) - len(valid),
        "spawned": sum(row["event"] == "worker started" for row in events),
        "serving_pids": len({row["pid"] for row in valid}),
        "request_starts": sum(row["event"] == "request started" for row in events),
        "request_ends": sum(row["event"] == "request finished" for row in events),
        "readable_events": len(notices),
        "readable_per_second": round(len(notices) * 1e9 / (end - start), 3),
        "readable_bins_pct": round(100 * len(occupied_bins) / math.ceil((end - start) / 50_000_000), 3),
        "longest_readable_run_ms": round(longest / 1e6, 3),
        "queue_alarm_ms": first_alarm,
        "accepts_observed": len(accepts),
        "event_next_accept_p95_ms": percentile(future_accept, .95),
        "event_nearest_accept_p95_ms": percentile(nearest_accept, .95),
        "unmatched_events": len(notices) - len(future_accept),
        "connect_accept_p95_ms": percentile(accept_waits, .95),
        "dispatch_start_p50_ms": percentile(waits, .5),
        "dispatch_start_p95_ms": percentile(waits, .95),
        "latency_p95_ms": percentile([(int(row["done_ns"]) - int(row["scheduled_ns"])) / 1e6 for row in valid], .95),
        "dispatch_lag_p95_ms": percentile([(int(row["dispatched_ns"]) - int(row["scheduled_ns"])) / 1e6 for row in valid], .95),
    }
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("--platform", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--latest", type=int, default=2, help="completed runs to keep per case")
    args = parser.parse_args()
    groups = {}
    for path in args.root.glob("*/scenario.csv"):
        if not (path.parent / "events.csv").exists():
            continue
        name = read(path)[0]["name"]
        groups.setdefault(name, []).append(path.parent)
    rows = []
    for name, directories in sorted(groups.items()):
        for directory in sorted(directories, key=lambda path: (path / "scenario.csv").stat().st_mtime)[-args.latest:]:
            rows.append(summarize(directory, args.platform))
    if not rows:
        raise SystemExit("no complete measurements")
    with args.output.open("w", newline="", encoding="utf-8") as stream:
        writer = csv.DictWriter(stream, fieldnames=rows[0].keys())
        writer.writeheader()
        writer.writerows(rows)
    for row in rows:
        print(f"{row['case']:30} p95_wait={row['dispatch_start_p95_ms']:>9}ms "
              f"events/s={row['readable_per_second']:>6} run={row['longest_readable_run_ms']:>8}ms "
              f"workers={row['spawned']}/{row['serving_pids']} errors={row['errors']}")


if __name__ == "__main__":
    main()
