"""Exercise saved-plan boundaries without credentials or cloud access."""

import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("deployment_iam_plan.py")
SPEC = importlib.util.spec_from_file_location("deployment_iam_plan", SCRIPT)
GUARD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARD)


def resource(address="google_service_account.runtime", resource_type="google_service_account",
             actions=None, mode="managed"):
    return {
        "address": address,
        "mode": mode,
        "type": resource_type,
        "change": {"actions": actions if actions is not None else ["create"],
                   "before": {"private_value": "must-not-be-logged"},
                   "after": {"private_value": "must-not-be-logged"}},
    }


def plan(*changes):
    return {"resource_changes": list(changes)}


class DeploymentIamPlanTests(unittest.TestCase):
    def test_identity_role_and_key_mutations_require_administrator(self):
        for kind in ("google_service_account", "google_service_account_key",
                     "google_project_iam_custom_role", "google_organization_iam_custom_role",
                     "google_service_account_iam_member", "google_service_account_iam_binding",
                     "google_service_account_iam_policy", "google_service_account_iam_future"):
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, "administrator bootstrap required"):
                GUARD.check(plan(resource(resource_type=kind)))

    def test_replacements_deletion_update_and_forget_cannot_escape_guard(self):
        for actions in (["create"], ["update"], ["delete"], ["delete", "create"],
                        ["create", "delete"], ["forget"]):
            with self.subTest(actions=actions), self.assertRaises(ValueError):
                GUARD.check(plan(resource(actions=actions)))

    def test_nested_module_instance_is_detected(self):
        address = 'module.targets["east"].module.runtime.google_service_account_iam_member.deploy[0]'
        self.assertEqual(GUARD.bootstrap_targets(plan(resource(address=address))), [address])

    def test_no_op_and_data_reads_are_safe_even_for_protected_types(self):
        GUARD.check(plan(resource(actions=["no-op"]),
                         resource(address="data.google_service_account.runtime",
                                  actions=["read"], mode="data"),
                         resource(address="google_service_account.refresh", actions=["read"])))

    def test_routine_resource_changes_remain_allowed(self):
        GUARD.check(plan(resource(address="google_cloud_run_v2_job.canary",
                                  resource_type="google_cloud_run_v2_job"),
                         resource(address="google_kms_crypto_key_iam_member.runtime",
                                  resource_type="google_kms_crypto_key_iam_member")))

    def test_bootstrap_rejects_mixed_vm_and_iam_mutations(self):
        with self.assertRaisesRegex(ValueError, "unrelated mutations"):
            GUARD.check_bootstrap(plan(resource(),
                                      resource(address="google_compute_instance.host",
                                               resource_type="google_compute_instance", actions=["update"])))

    def test_bootstrap_allows_iam_mutations_and_other_no_ops(self):
        GUARD.check_bootstrap(plan(resource(),
                                  resource(address="google_compute_instance.host",
                                           resource_type="google_compute_instance", actions=["no-op"]),
                                  resource(address="data.google_project.current",
                                           resource_type="google_project", mode="data", actions=["read"])))

    def test_bootstrap_targets_include_only_protected_mutations(self):
        a = resource(address="google_service_account.z")
        b = resource(address="google_project_iam_custom_role.a", resource_type="google_project_iam_custom_role")
        c = resource(address="google_compute_instance.host", resource_type="google_compute_instance")
        d = resource(address="google_service_account.unchanged", actions=["no-op"])
        self.assertEqual(GUARD.bootstrap_targets(plan(a, b, c, d)),
                         ["google_project_iam_custom_role.a", "google_service_account.z"])

    def test_malformed_or_errored_plan_fails_closed(self):
        for value in (None, [], {}, {"resource_changes": None}, {"resource_changes": {}},
                      {"resource_changes": [], "errored": True},
                      {"resource_changes": [], "errored": "false"}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                GUARD.check(value)

    def test_missing_change_metadata_or_unknown_actions_fail_closed(self):
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
        invalid.extend((None, [], {"address": "x"}))
        for item in invalid:
            with self.subTest(item=item), self.assertRaises(ValueError):
                GUARD.check(plan(item))

    def test_bad_mode_and_invalid_data_mutations_fail_closed(self):
        for item in (resource(mode="other"), resource(mode=None),
                     resource(mode="data", actions=["create"])):
            with self.subTest(item=item), self.assertRaises(ValueError):
                GUARD.check(plan(item))

    def test_duplicate_addresses_are_rejected(self):
        a = resource()
        b = copy.deepcopy(a)
        b["change"]["actions"] = ["no-op"]
        with self.assertRaisesRegex(ValueError, "duplicate"):
            GUARD.check(plan(a, b))

    def test_unsafe_address_and_type_are_rejected(self):
        for key, value in (("address", ""), ("address", "resource.name\nforged"),
                           ("address", "resource.name\x1b"), ("type", "google service account"),
                           ("type", None)):
            item = resource(resource_type="google_compute_instance")
            item[key] = value
            with self.subTest(key=key, value=value), self.assertRaisesRegex(ValueError, "address|type"):
                GUARD.check(plan(item))

    def test_cli_supports_stdin_for_all_modes(self):
        for option, value, expected in (
            ("--check", plan(resource(actions=["no-op"])), ""),
            ("--check-bootstrap", plan(resource()), ""),
            ("--bootstrap-targets", plan(resource()), "-target=google_service_account.runtime\n"),
        ):
            with self.subTest(option=option):
                result = subprocess.run([sys.executable, str(SCRIPT), option, "-"],
                                        input=json.dumps(value), capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, expected)

    def test_shell_target_output_is_inert_and_round_trips_as_one_argument(self):
        address = 'module.targets["a;$(touch /tmp/should-not-exist)"].google_service_account.runtime'
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plan.json"
            path.write_text(json.dumps(plan(resource(address=address))))
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                self.assertEqual(GUARD.main(["--bootstrap-targets", str(path)]), 0)
            self.assertEqual(shlex.split(output.getvalue()), ["-target=" + address])

    def test_cli_failure_does_not_print_attributes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plan.json"
            path.write_text(json.dumps(plan(resource())))
            result = subprocess.run([sys.executable, str(SCRIPT), "--check", str(path)],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("--check-bootstrap", result.stderr)
            self.assertNotIn("must-not-be-logged", result.stdout + result.stderr)

    def test_cli_bootstrap_rejects_resource_changes_and_json_parse_errors_without_values(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plan.json"
            path.write_text(json.dumps(plan(resource(address="google_compute_instance.host",
                                                     resource_type="google_compute_instance"))))
            result = subprocess.run([sys.executable, str(SCRIPT), "--check-bootstrap", str(path)],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertNotIn("must-not-be-logged", result.stdout + result.stderr)
            path.write_text('{"private_value": "must-not-be-logged", broken')
            result = subprocess.run([sys.executable, str(SCRIPT), "--check", str(path)],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stderr.strip(), "cannot read valid plan JSON")


if __name__ == "__main__":
    unittest.main()
