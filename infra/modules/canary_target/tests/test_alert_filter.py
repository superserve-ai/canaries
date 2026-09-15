"""Focused checks of the Terraform terminal filter against Cloud Logging envelopes.

This deliberately supports only ANDed string equalities, not the full Logging
query language. Unsupported syntax fails instead of silently weakening a test.
"""

import copy
import json
from pathlib import Path
import re
import unittest
from urllib.parse import unquote


MODULE = Path(__file__).resolve().parents[1]
INFRA = MODULE.parents[1]
TARGETS = {
    "production-us-west2": "envs/production/main.tf",
    "production-us-east4": "envs/production/us-east4/main.tf",
    "staging-us-central1": "envs/staging/us-central1/main.tf",
}
JOB_REFERENCE = "${google_cloud_run_v2_job.lifecycle.name}"


def equalities(filter_text):
    predicates = []
    for index, line in enumerate(filter_text.strip().splitlines()):
        prefix = "" if index == 0 else r"AND\s+"
        match = re.fullmatch(prefix + r'([\w.]+)="([^"\n]*)"', line.strip())
        if match is None:
            raise AssertionError(f"Unsupported filter predicate: {line!r}")
        predicates.append(match.groups())
    if not predicates:
        raise AssertionError("Empty filter")
    return predicates


def matches(predicates, entry):
    for field, expected in predicates:
        value = entry
        for component in field.split("."):
            value = value.get(component) if isinstance(value, dict) else None
        if value != expected:
            return False
    return True


class TerminalAlertTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = (MODULE / "main.tf").read_text()
        cls.policy = cls.source.split(
            'resource "google_monitoring_alert_policy" "cloud_run_job_failed" {', 1
        )[1].split('\nresource "', 1)[0]
        cls.filter = re.search(
            r"filter\s*=\s*<<-EOT\n(.*?)\n\s*EOT", cls.policy, re.S
        ).group(1)
        cls.fixture = json.loads((MODULE / "tests/terminal_failure.json").read_text())

    def test_envelopes_for_each_instantiated_target(self):
        self.assertNotIn("severity", self.fixture)
        for target in TARGETS:
            job = f"api-canary-{target}"
            predicates = equalities(self.filter.replace(JOB_REFERENCE, job))
            failure = copy.deepcopy(self.fixture)
            failure["resource"]["labels"]["job_name"] = job
            cases = [("observed terminal failure", failure, True)]
            normalized = copy.deepcopy(failure)
            normalized["severity"] = "ERROR"
            cases.append(("normalized terminal failure", normalized, True))
            success = copy.deepcopy(normalized)
            success["jsonPayload"]["result"] = "success"
            cases.append(("terminal success even at ERROR", success, False))
            for name, payload, severity in [
                ("generic level error", {"level": "error"}, None),
                ("generic severity error", {"message": "request failed"}, "ERROR"),
                ("other failure event", {"message": "request failed", "result": "failure"}, "ERROR"),
                ("terminal without result", {"message": "lifecycle canary completed"}, "ERROR"),
            ]:
                entry = copy.deepcopy(failure)
                entry["jsonPayload"] = payload
                if severity:
                    entry["severity"] = severity
                cases.append((name, entry, False))
            wrong_job = copy.deepcopy(failure)
            wrong_job["resource"]["labels"]["job_name"] = f"api-canary-janitor-{target}"
            cases.append(("other job", wrong_job, False))
            wrong_resource = copy.deepcopy(failure)
            wrong_resource["resource"]["type"] = "cloud_run_revision"
            cases.append(("other resource", wrong_resource, False))
            for name, entry, expected in cases:
                with self.subTest(target=target, case=name):
                    self.assertEqual(matches(predicates, entry), expected)

    def test_responder_context_and_notification_strategy(self):
        extractors = dict(re.findall(
            r'(\w+)\s*=\s*"EXTRACT\((.*?)\)"', self.policy
        ))
        self.assertEqual(extractors, {
            "execution_name": r'labels.\"run.googleapis.com/execution_name\"',
            "failed_step": "jsonPayload.failed_step",
            "sandbox_id": "jsonPayload.sandbox_id",
        })
        for field in ("sandbox_id", "failed_step"):
            self.assertIn("$${log.extracted_label." + field + "}", self.policy)
        encoded = re.search(
            r'lifecycle_run_logs_query\s*=\s*format\(\s*"([^"]+)"', self.source
        ).group(1)
        query = unquote(encoded.replace("%%", "%").replace(
            "%s", self.fixture["resource"]["labels"]["job_name"]
        ).replace(
            "$${log.extracted_label.execution_name}",
            self.fixture["labels"]["run.googleapis.com/execution_name"],
        ))
        self.assertEqual(query, '\n'.join([
            'resource.type="cloud_run_job"',
            'resource.labels.job_name="api-canary-production-us-west2"',
            'labels."run.googleapis.com/execution_name"="api-canary-production-us-west2-7zmpp"',
        ]))
        self.assertIn(
            "https://console.cloud.google.com/logs/query;query=${local.lifecycle_run_logs_query};project=${var.project_id}",
            self.policy,
        )
        self.assertRegex(self.policy, r'period\s*=\s*"300s"')
        self.assertRegex(self.policy, r'auto_close\s*=\s*"1800s"')
        self.assertEqual(self.policy.count("condition_matched_log {"), 1)
        self.assertRegex(self.policy, r"notification_channels\s*=\s*var.notification_channel_ids")

    def test_targets_use_shared_module(self):
        for target, path in TARGETS.items():
            with self.subTest(target=target):
                root = INFRA / path
                source = root.read_text()
                lifecycle = source.split('module "lifecycle" {', 1)[1]
                module_path = re.search(r'source\s*=\s*"([^"]+)"', lifecycle).group(1)
                self.assertEqual((root.parent / module_path).resolve(), MODULE)
                self.assertIn(target, source)
                self.assertRegex(lifecycle, r"create_alerts\s*=\s*var.create_alerts")


if __name__ == "__main__":
    unittest.main()
