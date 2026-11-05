#!/usr/bin/env python3
"""Keep identity and custom-role mutations outside routine Terraform deployments."""

import argparse
import json
from pathlib import Path
import re
import shlex
import sys


_PROTECTED_TYPES = {
    "google_service_account",
    "google_service_account_key",
    "google_project_iam_custom_role",
    "google_organization_iam_custom_role",
}
_ACTIONS = {
    ("no-op",),
    ("read",),
    ("create",),
    ("update",),
    ("delete",),
    ("delete", "create"),
    ("create", "delete"),
    ("forget",),
}
_UNCHANGED = {("no-op",), ("read",)}
_TYPE = re.compile(r"[A-Za-z][A-Za-z0-9_]*")


def _protected(resource_type):
    return (resource_type in _PROTECTED_TYPES
            or resource_type.startswith("google_service_account_iam_"))


def _changes(plan):
    if not isinstance(plan, dict) or not isinstance(plan.get("resource_changes"), list):
        raise ValueError("plan must contain a resource_changes list")
    if plan.get("errored", False) is not False:
        raise ValueError("cannot use an errored or malformed plan")
    seen = set()
    result = []
    for item in plan["resource_changes"]:
        if not isinstance(item, dict):
            raise ValueError("resource change must be an object")
        address, resource_type, mode = (item.get(key) for key in ("address", "type", "mode"))
        if (not isinstance(address, str) or not address
                or any(ord(char) < 32 or ord(char) == 127 for char in address)):
            raise ValueError("resource change has a missing or unsafe address")
        if address in seen:
            raise ValueError("plan contains duplicate resource addresses")
        seen.add(address)
        if not isinstance(resource_type, str) or not _TYPE.fullmatch(resource_type):
            raise ValueError("resource change has a missing or invalid type")
        if mode not in ("managed", "data"):
            raise ValueError("resource change must identify managed or data mode")
        change = item.get("change")
        actions = change.get("actions") if isinstance(change, dict) else None
        if (not isinstance(actions, list) or not all(isinstance(action, str) for action in actions)
                or tuple(actions) not in _ACTIONS):
            raise ValueError("resource change has missing or unsupported actions")
        actions = tuple(actions)
        if mode == "data" and actions not in _UNCHANGED:
            raise ValueError("data resource has mutation actions")
        if mode == "managed" and actions not in _UNCHANGED:
            result.append((address, resource_type))
    return result


def bootstrap_targets(plan):
    """Return protected mutation addresses, without reading resource attributes."""
    return sorted(address for address, resource_type in _changes(plan) if _protected(resource_type))


def check(plan):
    blocked = bootstrap_targets(plan)
    if blocked:
        raise ValueError(
            "administrator bootstrap required for: " + ", ".join(blocked) + ". "
            "Use --bootstrap-targets to review targeted changes, validate the saved "
            "administrator plan with --check-bootstrap, apply it in the same Terraform "
            "root/state, then rerun deployment."
        )


def check_bootstrap(plan):
    blocked = [address for address, resource_type in _changes(plan) if not _protected(resource_type)]
    if blocked:
        raise ValueError("administrator bootstrap plan contains unrelated mutations: " + ", ".join(blocked))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true", help="reject administrator-only IAM changes before routine apply")
    mode.add_argument("--bootstrap-targets", action="store_true", help="print shell-quoted -target arguments for administrator review")
    mode.add_argument("--check-bootstrap", action="store_true", help="allow only identity/custom-role mutations in an administrator plan")
    parser.add_argument("plan", type=Path, help="terraform show -json SAVED_PLAN output, or - for stdin")
    args = parser.parse_args(argv)
    try:
        if str(args.plan) == "-":
            plan = json.load(sys.stdin)
        else:
            with args.plan.open(encoding="utf-8") as handle:
                plan = json.load(handle)
        if args.bootstrap_targets:
            for address in bootstrap_targets(plan):
                print(shlex.quote("-target=" + address))
        elif args.check_bootstrap:
            check_bootstrap(plan)
        else:
            check(plan)
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as error:
        # JSON parser messages can include source fragments; do not expose plan values.
        message = ("cannot read valid plan JSON" if isinstance(error, (OSError, UnicodeError, json.JSONDecodeError))
                   else str(error))
        print(message, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
