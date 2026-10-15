"""A service exhausting startup retries must not leave a broken container up."""
import os
import signal
import sys
from pathlib import Path

while True:
    print("READY", flush=True)
    header = sys.stdin.readline()
    if not header:
        break
    fields = dict(item.split(":", 1) for item in header.split())
    body = sys.stdin.read(int(fields["len"]))
    sys.stdout.write("RESULT 2\nOK")
    sys.stdout.flush()
    if Path("/run/haunt/updating").exists():
        print("Application startup failed during an update; allowing the updater to restore its backup.", file=sys.stderr, flush=True)
        continue
    print("A service exhausted startup retries; stopping the container: " + body, file=sys.stderr, flush=True)
    os.kill(os.getppid(), signal.SIGTERM)
