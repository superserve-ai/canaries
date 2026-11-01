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


class OverlapAlertTest(unittest.TestCase):
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
