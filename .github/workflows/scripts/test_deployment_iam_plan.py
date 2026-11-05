"""Exercise deployment IAM boundaries without credentials or cloud access."""

import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("deployment_iam_plan.py")
SPEC = importlib.util.spec_from_file_location("deployment_iam_plan", SCRIPT)
GUARD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARD)

DEPLOYER = "serviceAccount:deployer@example-project.iam.gserviceaccount.com"
RUNTIME = "projects/example-project/serviceAccounts/runtime@example-project.iam.gserviceaccount.com"
USER = "roles/iam.serviceAccountUser"
TOKEN = "roles/iam.serviceAccountTokenCreator"


def resource(kind="google_service_account", actions=None, before=None, after=None, unknown=None,
             address=None, mode="managed"):
    item = {"address": address or kind + ".example", "mode": mode, "type": kind,
            "change": {"actions": actions if actions is not None else ["create"],
                       "before": before, "after": after if after is not None else {"private_value": "do-not-log"}}}
    if unknown is not None:
        item["change"]["after_unknown"] = unknown
    return item


def plan(*changes):
    return {"resource_changes": list(changes)}


def member(role=USER, who=DEPLOYER):
    return {"service_account_id": RUNTIME, "role": role, "member": who}


def policy(*bindings):
    return {"policy_data": json.dumps({"bindings": list(bindings)})}


def target(value, field="project", name="example-project"):
    return dict(value, **{field: name})


def binding(role=USER, members=None, condition=None):
    result = {"role": role, "members": members if members is not None else [DEPLOYER]}
    if condition is not None:
        result["condition"] = condition
    return result


class DeploymentIamPlanTests(unittest.TestCase):
    def test_account_and_project_custom_role_lifecycle_remain_automated(self):
        for kind in ("google_service_account", "google_project_iam_custom_role"):
            for actions in (["create"], ["update"], ["delete"], ["delete", "create"], ["create", "delete"]):
                with self.subTest(kind=kind, actions=actions):
                    GUARD.check(plan(resource(kind, actions)))

    def test_account_scoped_deployment_and_token_creator_grants_are_allowed(self):
        for role in (USER, TOKEN):
            GUARD.check(plan(resource("google_service_account_iam_member", after=member(role))))
            GUARD.check(plan(resource("google_service_account_iam_binding", after=binding(role))))
            GUARD.check(plan(resource("google_service_account_iam_policy", after=policy(binding(role)))))

    def test_computed_new_account_targets_and_members_do_not_require_manual_setup(self):
        for kind, after, unknown in (
            ("google_service_account_iam_member", {"role": TOKEN},
             {"service_account_id": True, "member": True}),
            ("google_service_account_iam_binding", {"role": USER, "members": [None, DEPLOYER]},
             {"service_account_id": True, "members": [True, False]}),
        ):
            GUARD.check(plan(resource(kind, after=after, unknown=unknown)))

    def test_unresolved_account_policy_roles_fail_closed(self):
        for kind, after, unknown in (
            ("google_service_account_iam_member", member(), {"role": True}),
            ("google_service_account_iam_binding", binding(), {"role": True}),
            ("google_service_account_iam_policy", policy(binding()), {"policy_data": True}),
        ):
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, "unresolved"):
                GUARD.check(plan(resource(kind, after=after, unknown=unknown)))
        with self.assertRaises(ValueError):
            GUARD.check(plan(resource("google_service_account_iam_member", after=member(), unknown=True)))

    def test_account_policy_changes_reject_other_roles_and_public_principals(self):
        for kind, value in (
            ("google_service_account_iam_member", member("roles/iam.serviceAccountAdmin")),
            ("google_service_account_iam_binding", binding("roles/viewer")),
            ("google_service_account_iam_policy", policy(binding("roles/iam.workloadIdentityUser"))),
        ):
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, "only deployment"):
                GUARD.check(plan(resource(kind, after=value)))
        for public in ("allUsers", "allAuthenticatedUsers"):
            for unknown in ({}, {"member": True}):
                with self.subTest(public=public, unknown=unknown), self.assertRaisesRegex(ValueError, "public"):
                    GUARD.check(plan(resource("google_service_account_iam_member",
                                             after=member(who=public), unknown=unknown)))
            with self.assertRaisesRegex(ValueError, "public"):
                GUARD.check(plan(resource("google_service_account_iam_binding", after=binding(members=[public]))))
            with self.assertRaisesRegex(ValueError, "public"):
                GUARD.check(plan(resource("google_service_account_iam_policy", after=policy(binding(members=[public])))))

    def test_authoritative_account_policy_leaves_unrelated_roles_unchanged(self):
        existing = binding("roles/iam.workloadIdentityUser", ["principalSet://example/pool/repository/example"])
        GUARD.check(plan(resource("google_service_account_iam_policy", ["update"],
                                 before=target(policy(existing), "service_account_id", RUNTIME),
                                 after=target(policy(existing, binding(USER)), "service_account_id", RUNTIME))))
        with self.assertRaisesRegex(ValueError, "only deployment"):
            GUARD.check(plan(resource("google_service_account_iam_policy", ["update"],
                                     before=target(policy(existing), "service_account_id", RUNTIME),
                                     after=target(policy(binding(USER)), "service_account_id", RUNTIME))))

    def test_account_policy_replacement_cannot_remove_an_unsupported_role_during_apply(self):
        with self.assertRaisesRegex(ValueError, "only deployment"):
            GUARD.check(plan(resource("google_service_account_iam_member", ["delete", "create"],
                                     before=member("roles/iam.serviceAccountAdmin"), after=member(USER))))
        GUARD.check(plan(resource("google_service_account_iam_member", ["delete"],
                                 before=member("roles/iam.serviceAccountAdmin"))))

    def test_critical_project_folder_and_organization_grants_are_rejected(self):
        for scope in ("project", "folder", "organization"):
            for role in GUARD._CRITICAL_ROLES:
                for kind, value in (("member", member(role)), ("binding", binding(role)),
                                    ("policy", policy(binding(role)))):
                    with self.subTest(scope=scope, role=role, kind=kind), self.assertRaisesRegex(ValueError, "critical service-account grant"):
                        GUARD.check(plan(resource("google_" + scope + "_iam_" + kind, after=value)))

    def test_project_wide_user_is_rejected_while_account_scoped_user_is_allowed(self):
        scoped = resource("google_service_account_iam_member", after=member())
        GUARD.check(plan(scoped))
        broad = copy.deepcopy(scoped)
        broad["type"] = "google_project_iam_member"
        with self.assertRaisesRegex(ValueError, "critical"):
            GUARD.check(plan(broad))

    def test_critical_grant_cleanup_and_existing_unchanged_grants_are_allowed(self):
        for kind, value in (("member", member("roles/owner")), ("binding", binding("roles/owner")),
                            ("policy", policy(binding("roles/owner")))):
            GUARD.check(plan(resource("google_project_iam_" + kind, ["delete"], before=value)))
            GUARD.check(plan(resource("google_project_iam_" + kind, ["no-op"], before=value, after=value)))
        before = binding("roles/owner", [DEPLOYER, "user:operator@example.com"])
        after = binding("roles/owner", ["user:operator@example.com"])
        GUARD.check(plan(resource("google_project_iam_binding", ["update"], before=before, after=after)))

    def test_authoritative_project_policy_detects_new_grants_without_rejecting_unchanged_ones(self):
        owner = binding("roles/owner")
        GUARD.check(plan(resource("google_project_iam_policy", ["update"],
                                 before=target(policy(owner)),
                                 after=target(policy(owner, binding("roles/logging.logWriter"))))))
        with self.assertRaisesRegex(ValueError, "critical"):
            GUARD.check(plan(resource("google_project_iam_policy", ["update"],
                                     before=policy(binding("roles/logging.logWriter")),
                                     after=policy(owner, binding("roles/logging.logWriter")))))

    def test_changed_critical_condition_is_not_treated_as_an_existing_grant(self):
        before = binding("roles/owner", condition={"expression": "request.time < timestamp('2030-01-01T00:00:00Z')"})
        after = binding("roles/owner")
        with self.assertRaisesRegex(ValueError, "critical"):
            GUARD.check(plan(resource("google_project_iam_binding", ["update"], before=before, after=after)))
        with self.assertRaisesRegex(ValueError, "unresolved"):
            GUARD.check(plan(resource("google_project_iam_binding", ["update"], before=before, after=before,
                                     unknown={"condition": True})))

    def test_replacements_and_scope_changes_cannot_reuse_critical_grants(self):
        for scope, field, old, new in (("project", "project", "old-project", "new-project"),
                                      ("folder", "folder", "folders/123", "folders/456"),
                                      ("organization", "org_id", "123", "456")):
            for kind, values in (("member", member("roles/owner")),
                                 ("binding", binding("roles/owner")),
                                 ("policy", policy(binding("roles/owner")))):
                for actions, destination, unknown in ((["delete", "create"], old, {}),
                                                      (["create", "delete"], old, {}),
                                                      (["delete", "create"], new, {}),
                                                      (["update"], new, {}),
                                                      (["update"], old, {field: True}),
                                                      (["update"], None, {})):
                    with self.subTest(scope=scope, kind=kind, actions=actions,
                                      destination=destination, unknown=unknown), self.assertRaisesRegex(ValueError, "critical"):
                        GUARD.check(plan(resource("google_" + scope + "_iam_" + kind, actions,
                                                 before=target(values, field, old),
                                                 after=target(values, field, destination), unknown=unknown)))

    def test_in_place_same_scope_updates_can_remove_critical_members(self):
        other = "serviceAccount:other@example-project.iam.gserviceaccount.com"
        for scope, field, value in (("project", "project", "example-project"),
                                    ("folder", "folder", "folders/123"),
                                    ("organization", "org_id", "123")):
            before = binding("roles/owner", [DEPLOYER, other])
            after = binding("roles/owner", [DEPLOYER])
            for kind, previous, following in (("binding", before, after),
                                              ("policy", policy(before), policy(after))):
                with self.subTest(scope=scope, kind=kind):
                    GUARD.check(plan(resource("google_" + scope + "_iam_" + kind, ["update"],
                                             before=target(previous, field, value),
                                             after=target(following, field, value))))

    def test_authoritative_account_policy_cannot_move_unchanged_unsupported_grants(self):
        existing = binding("roles/iam.workloadIdentityUser", ["principalSet://example/pool/repository/example"])
        for actions, destination, unknown in ((["delete", "create"], RUNTIME, {}),
                                              (["create", "delete"], RUNTIME, {}),
                                              (["update"], "projects/example-project/serviceAccounts/other@example-project.iam.gserviceaccount.com", {}),
                                              (["update"], RUNTIME, {"service_account_id": True}),
                                              (["update"], None, {})):
            with self.subTest(actions=actions, destination=destination, unknown=unknown), self.assertRaisesRegex(ValueError, "only deployment"):
                GUARD.check(plan(resource("google_service_account_iam_policy", actions,
                                         before=target(policy(existing), "service_account_id", RUNTIME),
                                         after=target(policy(existing, binding(USER)), "service_account_id", destination),
                                         unknown=unknown)))
        GUARD.check(plan(resource("google_service_account_iam_policy", after=policy(binding(USER), binding(TOKEN)),
                                 unknown={"service_account_id": True})))

    def test_unknown_critical_members_and_roles_fail_closed(self):
        for after, unknown in (({"role": "roles/owner"}, {"member": True}),
                               ({"member": DEPLOYER}, {"role": True})):
            with self.subTest(after=after), self.assertRaisesRegex(ValueError, "unresolved"):
                GUARD.check(plan(resource("google_project_iam_member", after=after, unknown=unknown)))
        with self.assertRaisesRegex(ValueError, "unresolved"):
            GUARD.check(plan(resource("google_project_iam_policy", after=policy(), unknown={"policy_data": True})))

    def test_public_critical_grants_cannot_bypass_the_service_account_filter(self):
        for public in ("allUsers", "allAuthenticatedUsers"):
            with self.subTest(public=public), self.assertRaisesRegex(ValueError, "critical"):
                GUARD.check(plan(resource("google_project_iam_member", after=member("roles/editor", public))))

    def test_known_noncritical_roles_allow_computed_new_members(self):
        for role in ("roles/logging.logWriter", "projects/example-project/roles/deploymentPolicy"):
            GUARD.check(plan(resource("google_project_iam_member", after={"role": role}, unknown={"member": True})))

    def test_human_and_google_managed_service_agent_grants_remain_allowed(self):
        for who in ("user:operator@example.com", "group:operators@example.com",
                    "serviceAccount:service-123@gcp-sa-example.iam.gserviceaccount.com",
                    "serviceAccount:service-123@compute-system.iam.gserviceaccount.com",
                    "serviceAccount:123@cloudservices.gserviceaccount.com"):
            GUARD.check(plan(resource("google_project_iam_member", after=member("roles/editor", who))))
        for who in ("serviceAccount:service-123@example-project.iam.gserviceaccount.com",
                    "serviceAccount:123-compute@developer.gserviceaccount.com"):
            with self.assertRaisesRegex(ValueError, "critical"):
                GUARD.check(plan(resource("google_project_iam_member", after=member("roles/editor", who))))

    def test_permanent_keys_and_organization_role_mutations_are_rejected(self):
        for kind, word in (("google_service_account_key", "key"),
                           ("google_organization_iam_custom_role", "organization")):
            for actions in (["create"], ["update"], ["delete", "create"], ["create", "delete"], ["forget"]):
                with self.subTest(kind=kind, actions=actions), self.assertRaisesRegex(ValueError, word):
                    GUARD.check(plan(resource(kind, actions)))
            GUARD.check(plan(resource(kind, ["delete"])))
            GUARD.check(plan(resource(kind, ["no-op"])))

    def test_nested_module_instance_is_checked(self):
        address = 'module.targets["east"].module.runtime.google_project_iam_member.admin[0]'
        with self.assertRaisesRegex(ValueError, "critical"):
            GUARD.check(plan(resource("google_project_iam_member", after=member("roles/owner"), address=address)))

    def test_malformed_policy_payloads_fail_without_disclosing_the_payload(self):
        for data in (None, [], "do-not-log", '{"private_value":"do-not-log",broken',
                     json.dumps([]), json.dumps({"bindings": [None]})):
            with self.subTest(data=data), self.assertRaises(ValueError) as raised:
                GUARD.check(plan(resource("google_project_iam_policy", after={"policy_data": data})))
            self.assertNotIn("do-not-log", str(raised.exception))

    def test_malformed_or_errored_plan_fails_closed(self):
        for value in (None, [], {}, {"resource_changes": None}, {"resource_changes": {}},
                      {"resource_changes": [], "errored": True}, {"resource_changes": [], "errored": "false"}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                GUARD.check(value)

    def test_missing_metadata_or_unknown_actions_fail_closed(self):
        invalid = []
        for key in ("address", "type", "mode", "change"):
            item = resource()
            del item[key]
            invalid.append(item)
        for actions in (None, [], ["update", "delete"], ["unknown"], ["no-op", "create"], [42], "create"):
            item = resource()
            item["change"]["actions"] = actions
            invalid.append(item)
        item = resource()
        del item["change"]["actions"]
        invalid.append(item)
        invalid.extend((None, [], {"address": "x"}, resource(mode="other"), resource(mode="data")))
        for item in invalid:
            with self.subTest(item=item), self.assertRaises(ValueError):
                GUARD.check(plan(item))

    def test_duplicate_and_unsafe_addresses_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "duplicate"):
            GUARD.check(plan(resource(), resource()))
        for key, value in (("address", ""), ("address", "resource.name\nforged"),
                           ("address", "resource.name\x1b"), ("type", "google service account"), ("type", None)):
            item = resource()
            item[key] = value
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, "address|type"):
                GUARD.check(plan(item))

    def test_no_op_reads_and_other_workload_changes_remain_allowed(self):
        GUARD.check(plan(resource(actions=["no-op"]),
                         resource(address="data.google_service_account.runtime", mode="data", actions=["read"]),
                         resource("google_cloud_run_v2_job", ["update"]),
                         resource("google_kms_crypto_key_iam_member", ["create"])))

    def test_unsupported_account_policy_type_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "unsupported"):
            GUARD.check(plan(resource("google_service_account_iam_future")))

    def test_cli_stdin_and_file_failure_do_not_log_resource_values(self):
        result = subprocess.run([sys.executable, str(SCRIPT), "--check", "-"],
                                input=json.dumps(plan(resource())), capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")
        result = subprocess.run([sys.executable, str(SCRIPT), "--check", "-"],
                                input=json.dumps(plan(resource("google_service_account_key"))),
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("key", result.stderr)
        self.assertNotIn("do-not-log", result.stdout + result.stderr)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plan.json"
            path.write_text('{"private_value":"do-not-log",broken')
            result = subprocess.run([sys.executable, str(SCRIPT), "--check", str(path)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stderr.strip(), "cannot read valid plan JSON")


if __name__ == "__main__":
    unittest.main()
