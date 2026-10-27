#!/usr/bin/env python3
"""Reject missing dependencies or skipped integration coverage in regression CI."""
import json
import sys
from pathlib import Path


def check(path):
    # These entry points run explicitly in subsequent browser/standalone steps.
    allowed = {
        ("github.com/example-git/haunt-server/internal/clienttest", "TestClients"),
        ("github.com/example-git/haunt-server/cmd/haunt", "TestFirstRunBinary"),
        ("github.com/example-git/haunt-server/internal/httpapi", "TestHistoryScale"),
        ("github.com/example-git/haunt-server/internal/embeds", "TestLiveSocialProviders"),
    }
    passed = 0
    for line in Path(path).read_text().splitlines():
        event = json.loads(line)
        if event.get("Action") == "fail":
            raise ValueError(f"Failed: {event.get('Package')} {event.get('Test', '')}")
        if not event.get("Test"):
            continue
        if event.get("Action") == "skip" and (event["Package"], event["Test"]) not in allowed:
            raise ValueError(f"Required test skipped: {event['Package']} {event['Test']}")
        passed += event.get("Action") == "pass"
    if not passed:
        raise ValueError("No passing test results recorded")
    print(f"Verified {passed} passing tests; no unexpected skips.")


if __name__ == "__main__":
    check(sys.argv[1])
