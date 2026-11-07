"""Exercise daily-load alert filters with Cloud Logging envelopes and root wiring."""

import copy
from pathlib import Path
import re
import unittest


MODULE = Path(__file__).resolve().parents[1]
INFRA = MODULE.parents[1]
TARGETS = {
    "staging-us-central1": ("envs/staging/us-central1", "rayai-dev", "11010951251479934548"),
    "production-us-west2": ("envs/production", "rayai-prod", "7690949645937454026"),
    "production-us-east4": ("envs/production/us-east4", "rayai-prod", "7690949645937454026"),
}


def block(source, kind, name):
    """Extract a top-level block without interpreting nested HCL expressions."""
    start = source.index(f'{kind} "{name}" {{')
    end = re.search(r"^}\s*$", source[start:], re.M)
    return source[start:start + end.end()]


def matches(expression, entry):
    """Evaluate only this filter's equality/AND/OR grammar; reject other syntax."""
    tokens = re.findall(r'\s*(AND\b|OR\b|[()]|=|"[^"\n]*"|[\w.]+)', expression)
    if "".join(tokens) != re.sub(r"\s+", "", expression):
        # Quoted messages contain spaces, which must remain meaningful.
        compact = re.sub(r'"[^"\n]*"|\s+', lambda m: m[0] if m[0].startswith('"') else "", expression)
        if "".join(tokens) != compact:
            raise AssertionError(f"Unsupported filter syntax: {expression}")
    position = 0

    def atom():
        nonlocal position
        if tokens[position] == "(":
            position += 1
            value = disjunction()
            if tokens[position] != ")":
                raise AssertionError("Unclosed filter group")
            position += 1
            return value
        field, operator, expected = tokens[position:position + 3]
        position += 3
        if operator != "=" or not expected.startswith('"'):
            raise AssertionError("Only string equalities are supported")
        value = entry
        for part in field.split("."):
            value = value.get(part) if isinstance(value, dict) else None
        return value == expected[1:-1]

    def conjunction():
        nonlocal position
        value = atom()
        while position < len(tokens) and tokens[position] == "AND":
            position += 1
            right = atom()
            value = value and right
        return value

    def disjunction():
        nonlocal position
        value = conjunction()
        while position < len(tokens) and tokens[position] == "OR":
            position += 1
            right = conjunction()
            value = value or right
        return value

    result = disjunction()
    if position != len(tokens):
        raise AssertionError("Trailing filter syntax")
    return result


class DailyLoadAlertRoutingTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.alerts = (MODULE / "alerts.tf").read_text()
        cls.terminal = block(cls.alerts, 'resource "google_monitoring_alert_policy"', "terminal_failure")
        cls.filter = re.search(r"filter\s*=\s*<<-EOT\n(.*?)\n\s*EOT", cls.terminal, re.S)[1]

    def test_terminal_events_page_without_operation_error_storms(self):
        for target in TARGETS:
            job = f"sandbox-daily-load-{target}"
            expression = self.filter.replace("${google_cloud_run_v2_job.daily.name}", job)
            envelope = {"resource": {"type": "cloud_run_job", "labels": {"job_name": job}}}
            # Zerolog's level is retained inside jsonPayload; Cloud Logging's
            # top-level severity is not guaranteed to be normalized.
            events = [
                ({"message": "load test completed", "result": "failure", "failed": 1}, True),
                ({"message": "load test completed", "result": "failure", "cleanup_failures": 1}, True),
                ({"message": "load test completed", "result": "failure", "error": "summary delivery failed"}, True),
                ({"message": "load runner failed", "level": "error", "error": "invalid configuration"}, True),
                ({"message": "load test completed", "result": "success"}, False),
                ({"message": "load test completed"}, False),
                ({"message": "load-test cleanup failed; janitor will reclaim sandbox after expiry", "level": "error"}, False),
                ({"message": "load-test operation complete", "result": "failure", "level": "error"}, False),
                ({"message": "lifecycle canary completed", "result": "failure"}, False),
            ]
            for payload, expected in events:
                with self.subTest(target=target, event=payload):
                    entry = copy.deepcopy(envelope)
                    entry["jsonPayload"] = payload
                    self.assertEqual(matches(expression, entry), expected)
            other_job = copy.deepcopy(envelope)
            other_job["resource"]["labels"]["job_name"] = f"api-canary-{target}"
            other_job["jsonPayload"] = {"message": "load runner failed"}
            self.assertFalse(matches(expression, other_job))

    def test_roots_route_failures_to_oncall_independently_of_summaries(self):
        for target, (path, project, channel) in TARGETS.items():
            with self.subTest(target=target):
                root = INFRA / path
                source = (root / "main.tf").read_text()
                daily = block(source, "module", "daily_load")
                module_path = re.search(r'source\s*=\s*"([^"\n]*)"', daily)[1]
                self.assertEqual((root / module_path).resolve(), MODULE)
                self.assertRegex(daily, r"notification_channel_ids\s*=\s*var.oncall_notification_channel_ids")
                self.assertRegex(daily, r"image\s*=\s*var.load_runner_image")
                variables = (root / "variables.tf").read_text()
                oncall = block(variables, "variable", "oncall_notification_channel_ids")
                self.assertIn(f"projects/{project}/notificationChannels/{channel}", oncall)
                self.assertIn("slack_webhook_secret_name", daily)

        staging = (INFRA / "envs/staging/us-central1/main.tf").read_text()
        stage_daily = block(staging, "module", "daily_load")
        self.assertRegex(stage_daily, r"manage_slack_webhook_access\s*=\s*false")
        self.assertIn("projects/887554770957/secrets/canary-benchmark-slack-webhook", stage_daily)
        production = (INFRA / "envs/production/main.tf").read_text()
        grant = block(production, 'resource "google_secret_manager_secret_iam_member"', "staging_load_reporting_accessor")
        self.assertIn("sbx-load-runner-staging@rayai-dev.iam.gserviceaccount.com", grant)
        self.assertIn("google_secret_manager_secret.benchmark_slack_webhook.secret_id", grant)
        self.assertIn('"roles/secretmanager.secretAccessor"', grant)

    def test_all_policies_use_oncall_and_bounded_log_notifications(self):
        names = re.findall(r'resource "google_monitoring_alert_policy" "([^"\n]+)"', self.alerts)
        self.assertEqual(set(names), {"terminal_failure", "execution_failure", "scheduler_failure"})
        for name in names:
            policy = block(self.alerts, 'resource "google_monitoring_alert_policy"', name)
            self.assertRegex(policy, r"notification_channels\s*=\s*var.notification_channel_ids")
            if "condition_matched_log" in policy:
                self.assertRegex(policy, r'notification_rate_limit\s*{\s*period\s*=\s*"300s"')


if __name__ == "__main__":
    unittest.main()
