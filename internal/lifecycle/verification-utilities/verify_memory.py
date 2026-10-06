#!/usr/bin/env python3
import os
import sys


def ancestors() -> set:
    # The token rides in on this process's own command line (and its wrapper
    # shells'), so only processes outside that ancestry can prove a restore.
    seen = set()
    pid = os.getpid()
    while pid > 1 and pid not in seen:
        seen.add(pid)
        try:
            with open("/proc/%d/status" % pid) as fh:
                ppid = next(int(line.split()[1]) for line in fh if line.startswith("PPid:"))
        except (OSError, StopIteration, ValueError):
            break
        pid = ppid
    return seen


def main() -> int:
    token = os.environ.get("CANARY_MEMORY_TOKEN")
    if not token:
        print("missing CANARY_MEMORY_TOKEN", file=sys.stderr)
        return 2

    needle = token.encode()
    skip = ancestors()
    for pid in os.listdir("/proc"):
        if not pid.isdigit() or int(pid) in skip:
            continue
        try:
            with open("/proc/" + pid + "/cmdline", "rb") as fh:
                cmdline = fh.read().replace(b"\x00", b" ")
        except OSError:
            continue
        if needle in cmdline and b"verify_memory.py" not in cmdline:
            print("verified")
            return 0

    print("memory token process not found", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
