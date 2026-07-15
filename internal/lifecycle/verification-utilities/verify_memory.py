#!/usr/bin/env python3
import os
import sys


def main() -> int:
    token = os.environ.get("CANARY_MEMORY_TOKEN")
    if not token:
        print("missing CANARY_MEMORY_TOKEN", file=sys.stderr)
        return 2

    needle = token.encode()
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            with open("/proc/" + pid + "/cmdline", "rb") as fh:
                cmdline = fh.read().replace(b"\x00", b" ")
        except OSError:
            continue
        if needle in cmdline:
            print("verified")
            return 0

    print("memory token process not found", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
