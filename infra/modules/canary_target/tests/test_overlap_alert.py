"""Checks which targets page on a single lock skip and which wait for repeats or a stall."""

from pathlib import Path
import re
import unittest


MODULE = Path(__file__).resolve().parents[1]
INFRA = MODULE.parents[1]
PRODUCTION = ["envs/production/main.tf", "envs/production/us-east4/main.tf"]
STAGING = "envs/staging/us-central1/main.tf"


def block(source, header):
    body = source.split(header + " {", 1)[1]
    return body[: body.index("\n}\n")]


def setting(module, name):
    match = re.search(rf"^\s*{name}\s*=\s*(\S+)", module, re.M)
    return match and match.group(1)


def policies(source):
    parts = source.split('\nresource "google_monitoring_alert_policy" "')[1:]
    return {part.split('"', 1)[0]: part.split('\nresource "', 1)[0] for part in parts}


class OverlapAlertTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.policies = policies((MODULE / "main.tf").read_text())

    def test_promql_policies_have_one_condition(self):
        # Cloud Monitoring rejects a PromQL condition that shares its policy with another condition.
        for name, policy in self.policies.items():
            if "condition_prometheus_query_language" not in policy:
                continue
            with self.subTest(policy=name):
                self.assertEqual(len(re.findall(r"^  (?:conditions|dynamic \"conditions\") \{", policy, re.M)), 1)
                self.assertEqual(policy.count("condition_prometheus_query_language {"), 1)

    def test_default_repeat_query_matches_single_skip_alert(self):
        query = re.search(r'query\s*=\s*"(.*)"\n', self.policies["overlap_skipped"]).group(1)
        rendered = query.replace(
            "${local.overlap_skip_selector}",
            'superserve_canary_overlap_skipped_total{target=\\"${var.target_name}\\",scenario=\\"${var.scenario}\\"}',
        ).replace("${var.overlap_skip_window}", "15m").replace("${var.overlap_skip_min_count - 1}", "0")
        self.assertEqual(
            rendered,
            '((sum(max_over_time(superserve_canary_overlap_skipped_total{target=\\"${var.target_name}\\",scenario=\\"${var.scenario}\\"}[15m])) or vector(0)) > 0)',
        )
        self.assertRegex(self.policies["overlap_skipped"], r'duration\s*=\s*"0s"')

    def test_stall_policy_is_opt_in_with_hold(self):
        policy = self.policies["overlap_stalled"]
        self.assertRegex(policy, r"count\s*=\s*var.create_alerts && var.overlap_skip_page_without_completion \? 1 : 0")
        self.assertRegex(policy, r'duration\s*=\s*"300s"')
        self.assertIn("and on()", policy)
        self.assertIn("[${var.missing_runs_window}]", policy)
        self.assertIn("precondition {", policy)

    def test_module_defaults_keep_single_skip_alert(self):
        variables = (MODULE / "variables.tf").read_text()
        for name, default in [
            ("overlap_skip_min_count", "1"),
            ("overlap_skip_window", '"15m"'),
            ("overlap_skip_page_without_completion", "false"),
        ]:
            with self.subTest(variable=name):
                self.assertEqual(setting(block(variables, f'variable "{name}"'), "default"), default)

    def test_production_keeps_single_skip_alert(self):
        for path in PRODUCTION:
            with self.subTest(path=path):
                self.assertNotIn("overlap_skip_", (INFRA / path).read_text())

    def test_staging_pages_on_repeat_or_stall(self):
        source = (INFRA / STAGING).read_text()
        for scenario in ("lifecycle", "snapshot"):
            module = block(source, f'module "{scenario}"')
            with self.subTest(scenario=scenario):
                self.assertEqual(setting(module, "overlap_skip_min_count"), "2")
                self.assertEqual(setting(module, "overlap_skip_window"), '"60m"')
                self.assertEqual(setting(module, "overlap_skip_page_without_completion"), "true")


if __name__ == "__main__":
    unittest.main()
