#!/usr/bin/env python3
"""Reject key creation and critical predefined grants in deployment plans."""

import argparse
import json
from pathlib import Path
import re
import sys


_CRITICAL_ROLES = {
    "roles/owner", "roles/editor", "roles/viewer", "roles/billing.admin",
    "roles/iam.organizationRoleAdmin", "roles/iam.roleAdmin", "roles/iam.securityAdmin",
    "roles/iam.serviceAccountAdmin", "roles/iam.serviceAccountKeyAdmin",
    "roles/iam.serviceAccountUser", "roles/iam.serviceAccountTokenCreator",
    "roles/resourcemanager.organizationAdmin",
    "roles/resourcemanager.folderAdmin", "roles/resourcemanager.folderEditor",
    "roles/resourcemanager.folderMover",
}
_ACCOUNT_POLICY_ROLES = {"roles/iam.serviceAccountUser", "roles/iam.serviceAccountTokenCreator"}
_ACTIONS = {
    ("no-op",), ("read",), ("create",), ("update",), ("delete",),
    ("delete", "create"), ("create", "delete"), ("forget",),
}
_UNCHANGED = {("no-op",), ("read",)}
_TYPE = re.compile(r"[A-Za-z][A-Za-z0-9_]*")
_PROJECT_POLICY = re.compile(r"google_(project|folder|organization)_iam_(member|binding|policy)")
_ACCOUNT_POLICY = re.compile(r"google_service_account_iam_(member|binding|policy)")
_GOOGLE_SERVICE_AGENT = re.compile(
    r"service-\d+@(?:gcp-sa-[a-z0-9-]+|compute-system|container-engine-robot|"
    r"serverless-robot-prod|containerregistry|dataflow-service-producer-prod|"
    r"cloud-filer|cloud-redis)\.iam\.gserviceaccount\.com"
)
_PUBLIC_MEMBERS = {"allUsers", "allAuthenticatedUsers"}


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
            result.append((address, resource_type, actions, change))
    return result


def _unknown(value):
    if isinstance(value, dict):
        return any(_unknown(child) for child in value.values())
    if isinstance(value, list):
        return any(_unknown(child) for child in value)
    return value is not False and value is not None


def _field_unknown(change, field):
    value = change.get("after_unknown", {})
    if not isinstance(value, dict):
        raise ValueError("IAM change has unresolved or malformed attributes")
    return _unknown(value.get(field))


def _same_target_before(change, actions, field):
    # A replacement or a different target creates grants even if its policy
    # content happens to match the old resource.
    if actions != ("update",) or _field_unknown(change, field):
        return None
    before, after = change.get("before"), change.get("after")
    if not isinstance(before, dict) or not isinstance(after, dict):
        return None
    target = after.get(field)
    if not isinstance(target, str) or not target or before.get(field) != target:
        return None
    return before


def _role(value):
    if not isinstance(value, str) or not value or any(char.isspace() for char in value):
        raise ValueError("IAM change has a missing or invalid role")
    return value


def _members(value):
    if (not isinstance(value, list) or not all(isinstance(member, str) and member for member in value)):
        raise ValueError("IAM change has missing or malformed members")
    return set(value)


def _condition(value):
    if value is None or value == []:
        return ""
    if isinstance(value, list) and len(value) == 1:
        value = value[0]
    if not isinstance(value, dict):
        raise ValueError("IAM change has a malformed condition")
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def _bindings(value, kind):
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise ValueError("IAM change has malformed policy attributes")
    if kind == "policy":
        data = value.get("policy_data")
        if not isinstance(data, str):
            raise ValueError("IAM change has missing or unresolved policy data")
        try:
            policy = json.loads(data)
        except (json.JSONDecodeError, ValueError):
            raise ValueError("IAM change has invalid policy data") from None
        if not isinstance(policy, dict) or not isinstance(policy.get("bindings", []), list):
            raise ValueError("IAM change has malformed policy bindings")
        items = policy.get("bindings", [])
    else:
        items = [value]
    result = {}
    for binding in items:
        if not isinstance(binding, dict):
            raise ValueError("IAM change has a malformed binding")
        role = _role(binding.get("role"))
        members = _members([binding.get("member")] if kind == "member" else binding.get("members"))
        key = role, _condition(binding.get("condition"))
        result.setdefault(key, set()).update(members)
    return result


def _user_managed(member):
    if not member.startswith("serviceAccount:"):
        return False
    email = member[len("serviceAccount:"):]
    if _GOOGLE_SERVICE_AGENT.fullmatch(email):
        return False
    if re.fullmatch(r"\d+@(?:cloudservices|cloudbuild)\.gserviceaccount\.com", email):
        return False
    return True


def _check_project_policy(change, kind, actions, scope):
    after = change.get("after")
    if not isinstance(after, dict):
        raise ValueError("IAM change has missing policy attributes")
    if kind != "policy":
        if _field_unknown(change, "role"):
            raise ValueError("IAM change has an unresolved role")
        role = _role(after.get("role"))
        # Unknown generated members are harmless to this check when the known
        # role is outside the prohibited set.
        if role not in _CRITICAL_ROLES:
            return
    for field in (("policy_data",) if kind == "policy" else ("member" if kind == "member" else "members", "condition")):
        if _field_unknown(change, field):
            raise ValueError("critical IAM change has unresolved grant attributes")
    field = {"project": "project", "folder": "folder", "organization": "org_id"}[scope]
    before = _bindings(_same_target_before(change, actions, field), kind)
    after = _bindings(after, kind)
    for (role, condition), members in after.items():
        if role in _CRITICAL_ROLES:
            added = members - before.get((role, condition), set())
            if any(_user_managed(member) or member in _PUBLIC_MEMBERS for member in added):
                raise ValueError("critical service-account grant is outside the deployment permission contract")


def _check_account_policy(change, kind, actions):
    after = change.get("after")
    if not isinstance(after, dict):
        raise ValueError("service-account IAM change has missing policy attributes")
    if kind == "policy":
        if _field_unknown(change, "policy_data"):
            raise ValueError("service-account IAM change has unresolved policy data")
        before = _bindings(_same_target_before(change, actions, "service_account_id"), kind)
        after = _bindings(after, kind)
        for key in before.keys() | after.keys():
            if before.get(key, set()) == after.get(key, set()):
                continue
            if key[0] not in _ACCOUNT_POLICY_ROLES:
                raise ValueError("service-account policy changes may grant only deployment and token-creator roles")
            if after.get(key, set()) & _PUBLIC_MEMBERS:
                raise ValueError("service-account policy cannot grant access to public principals")
        return
    if _field_unknown(change, "role"):
        raise ValueError("service-account IAM change has an unresolved role")
    role = _role(after.get("role"))
    if role not in _ACCOUNT_POLICY_ROLES:
        raise ValueError("service-account policy changes may grant only deployment and token-creator roles")
    before = change.get("before")
    if before is not None:
        if not isinstance(before, dict) or _role(before.get("role")) not in _ACCOUNT_POLICY_ROLES:
            raise ValueError("service-account policy changes may modify only deployment and token-creator roles")
    member_field = "member" if kind == "member" else "members"
    if _field_unknown(change, member_field):
        raise ValueError("service-account IAM change has unresolved policy recipients")
    members = _members([after.get("member")] if kind == "member" else after.get("members"))
    if members & _PUBLIC_MEMBERS:
        raise ValueError("service-account policy cannot grant access to public principals")


def check(plan):
    for address, resource_type, actions, change in _changes(plan):
        try:
            if actions == ("delete",):
                continue
            if resource_type == "google_service_account_key":
                raise ValueError("service-account key creation or mutation is outside the deployment permission contract")
            if resource_type == "google_organization_iam_custom_role":
                raise ValueError("organization custom-role mutation is outside the deployment permission contract")
            project_policy = _PROJECT_POLICY.fullmatch(resource_type)
            account_policy = _ACCOUNT_POLICY.fullmatch(resource_type)
            if project_policy:
                _check_project_policy(change, project_policy[2], actions, project_policy[1])
            elif account_policy:
                _check_account_policy(change, account_policy[1], actions)
            elif resource_type.startswith("google_service_account_iam_"):
                raise ValueError("unsupported service-account policy resource")
        except ValueError as error:
            raise ValueError(address + ": " + str(error)) from None


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", required=True,
                        help="validate IAM changes before applying the checked saved plan")
    parser.add_argument("plan", type=Path, help="terraform show -json SAVED_PLAN output, or - for stdin")
    args = parser.parse_args(argv)
    try:
        if str(args.plan) == "-":
            plan = json.load(sys.stdin)
        else:
            with args.plan.open(encoding="utf-8") as handle:
                plan = json.load(handle)
        check(plan)
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError) as error:
        message = ("cannot read valid plan JSON" if isinstance(error, (OSError, UnicodeError, json.JSONDecodeError))
                   else str(error))
        print(message, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
