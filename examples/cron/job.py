"""An ordinary finite command: scheduled jobs do not require worker telemetry."""

import os
import time

print(f"job started pid={os.getpid()}", flush=True)
time.sleep(float(os.environ.get("JOB_SECONDS", "2")))
print(f"job finished pid={os.getpid()}", flush=True)
