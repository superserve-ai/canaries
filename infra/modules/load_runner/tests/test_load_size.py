"""Pin the daily load size per target so staging stays small and production does not drift."""

import re
import unittest

from test_alert_routing import INFRA, MODULE, block


def number(source, name):
    match = re.search(rf"^\s*{name}\s*=\s*(\d+)\s*$", source, re.M)
    return int(match[1]) if match else None


class DailyLoadSizeTest(unittest.TestCase):
    def test_module_defaults_are_the_production_size(self):
        variables = (MODULE / "variables.tf").read_text()
        self.assertEqual(number(block(variables, "variable", "load_operations"), "default"), 1000)
        self.assertEqual(number(block(variables, "variable", "load_concurrency"), "default"), 100)

    def test_module_passes_the_size_to_the_runner_and_scheduler(self):
        main = (MODULE / "main.tf").read_text()
        self.assertRegex(main, r"LOAD_TEST_OPERATIONS\s*=\s*tostring\(var\.load_operations\)")
        self.assertRegex(main, r"LOAD_TEST_CONCURRENCY\s*=\s*tostring\(var\.load_concurrency\)")
        self.assertNotRegex(main, r'LOAD_TEST_(OPERATIONS|CONCURRENCY)\s*=\s*"\d+"')
        self.assertIn("${var.load_operations}-operation / ${var.load_concurrency}-concurrent", main)

    def test_production_roots_keep_the_module_defaults(self):
        for path in ("envs/production", "envs/production/us-east4"):
            with self.subTest(root=path):
                daily = block((INFRA / path / "main.tf").read_text(), "module", "daily_load")
                self.assertNotIn("load_operations", daily)
                self.assertNotIn("load_concurrency", daily)

    def test_staging_uses_a_size_its_hosts_can_finish(self):
        staging = (INFRA / "envs/staging/us-central1/main.tf").read_text()
        daily = block(staging, "module", "daily_load")
        operations, concurrency = number(daily, "load_operations"), number(daily, "load_concurrency")
        self.assertEqual((operations, concurrency), (100, 10))
        # Keep the daily job in line with the manual staging load job's defaults.
        manual = block(staging, 'resource "google_cloud_run_v2_job"', "load_runner")
        for name, value in (("LOAD_TEST_OPERATIONS", operations), ("LOAD_TEST_CONCURRENCY", concurrency)):
            self.assertRegex(manual, rf'name\s*=\s*"{name}"\s*\n\s*value\s*=\s*"{value}"')


if __name__ == "__main__":
    unittest.main()
