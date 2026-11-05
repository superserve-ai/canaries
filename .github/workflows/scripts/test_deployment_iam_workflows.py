"""Execute deployment run blocks with Terraform isolated from cloud access."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[3]


def _run_block(workflow, job, step):
    source = workflow.read_text()
    match = re.search(r"^  " + re.escape(job) + r":\n(?P<body>.*?)(?=^  [a-zA-Z0-9_-]+:|\Z)",
                      source, re.M | re.S)
    if match is None:
        raise AssertionError("missing workflow job: " + job)
    steps = match["body"].split("      - name: " + step + "\n", 1)
    if len(steps) != 2:
        raise AssertionError("missing workflow step: " + step)
    match = re.search(r"^        run: \|\n((?:          [^\n]*\n|\s*\n)+)", steps[1], re.M)
    if match is None:
        raise AssertionError("missing workflow run block: " + step)
    script = textwrap.dedent(match[1])
    if "${{" in script:
        raise AssertionError("unresolved workflow expression in test run block")
    return script


def _change(kind, actions):
    return {"address": kind + ".example", "type": kind, "mode": "managed",
            "change": {"actions": actions, "before": {}, "after": {}}}


# The fake applies only a saved artifact written by its own plan command.
# Show can fail after emitting valid JSON to exercise the workflow's pipefail.
TERRAFORM = """#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
args = sys.argv[1:]
with open(os.environ["TF_TEST_CALLS"], "a") as log:
    log.write(json.dumps({"args": args, "cwd": os.getcwd()}) + "\\n")
if args[0] in ("init", "validate"):
    raise SystemExit(0)
if args[0] == "plan":
    output = next((arg.split("=", 1)[1] for arg in args if arg.startswith("-out=")), None)
    if output is None:
        raise SystemExit("test requires a saved plan")
    Path(output).write_text(os.environ["TF_TEST_PLAN"])
elif args[0] == "show":
    if args[1] != "-json":
        raise SystemExit("test requires JSON show")
    sys.stdout.write(Path(args[-1]).read_text())
    raise SystemExit(int(os.environ.get("TF_TEST_SHOW_EXIT", "0")))
elif args[0] == "apply":
    saved = [arg for arg in args[1:] if not arg.startswith("-")]
    if len(saved) != 1 or Path(saved[0]).read_text() != os.environ["TF_TEST_PLAN"]:
        raise SystemExit("apply did not consume the checked saved plan")
    Path(os.environ["TF_TEST_APPLIED"]).write_text(str(Path(saved[0]).resolve()))
else:
    raise SystemExit("unexpected Terraform command")
"""


class DeploymentIamWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        workflow = ROOT / ".github/workflows/terraform-cd.yml"
        if workflow.exists():
            cls.workflow = workflow
            cls.cases = (
                ("production-us-central1-bootstrap",
                 "Apply production/us-central1 project services and CD permissions", "google_project_service"),
                ("production-us-central1-infra",
                 "Terraform apply production/us-central1", "google_monitoring_dashboard"),
            )
        else:
            cls.workflow = ROOT / ".github/workflows/deploy.yml"
            cls.cases = (
                ("deploy-staging", "Apply staging", "google_cloud_run_v2_job"),
                ("deploy-production", "Apply production", "google_cloud_run_v2_job"),
                ("deploy-production", "Apply production east", "google_cloud_run_v2_job"),
            )

    def _execute(self, job, step, fixture, show_exit=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for relative in ("infra/envs/staging/us-central1", "infra/envs/production/us-central1",
                             "infra/envs/production/us-east4", "bin"):
                (root / relative).mkdir(parents=True, exist_ok=True)
            command = root / "bin/terraform"
            command.write_text(TERRAFORM)
            command.chmod(0o755)
            calls = root / "calls.jsonl"
            applied = root / "applied"
            env = dict(os.environ, PATH=str(root / "bin") + os.pathsep + os.environ["PATH"],
                       GITHUB_WORKSPACE=str(ROOT), GITHUB_STEP_SUMMARY=str(root / "summary"),
                       RUNNER_TEMP=str(root), TF_TEST_CALLS=str(calls),
                       TF_TEST_APPLIED=str(applied), TF_TEST_PLAN=fixture,
                       TF_TEST_SHOW_EXIT=str(show_exit), DEPLOY_SCOPE="full",
                       API_IMAGE="example.invalid/api:release", UI_IMAGE="example.invalid/ui:release")
            result = subprocess.run(["bash", "-c", _run_block(self.workflow, job, step)],
                                    cwd=root, env=env, text=True, capture_output=True)
            records = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
            return result, records, applied.read_text() if applied.exists() else None

    def test_protected_iam_diff_aborts_before_apply(self):
        fixture = json.dumps({"resource_changes": [
            _change("google_service_account_iam_member", ["create"]),
        ]})
        for job, step, _ in self.cases:
            with self.subTest(step=step):
                result, records, applied = self._execute(job, step, fixture)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("administrator bootstrap required", result.stderr)
                self.assertIsNone(applied)
                self.assertFalse(any(record["args"][0] == "apply" for record in records))

    def test_allowed_workload_diff_applies_the_checked_saved_plan(self):
        for job, step, kind in self.cases:
            fixture = json.dumps({"resource_changes": [
                _change("google_service_account", ["no-op"]),
                _change(kind, ["update"]),
            ]})
            with self.subTest(step=step):
                result, records, applied = self._execute(job, step, fixture)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                plan = next(record for record in records if record["args"][0] == "plan")
                shows = [record for record in records if record["args"][0] == "show"]
                applies = [record for record in records if record["args"][0] == "apply"]
                self.assertEqual(len(applies), 1)
                filename = next(arg.split("=", 1)[1] for arg in plan["args"] if arg.startswith("-out="))
                self.assertTrue(shows)
                self.assertTrue(all(record["args"][-1] == filename for record in shows))
                self.assertEqual(applies[0]["args"][-1], filename)
                self.assertEqual(applied, str(Path(applies[0]["cwd"]) / filename))
                self.assertFalse(any(arg.startswith("-var") or arg.startswith("-target")
                                     for arg in applies[0]["args"]))

    def test_show_failure_with_valid_json_cannot_reach_apply(self):
        fixture = json.dumps({"resource_changes": []})
        for job, step, _ in self.cases:
            with self.subTest(step=step):
                result, records, applied = self._execute(job, step, fixture, show_exit=42)
                self.assertEqual(result.returncode, 42, result.stdout + result.stderr)
                self.assertTrue(any(record["args"][0] == "show" for record in records))
                self.assertIsNone(applied)
                self.assertFalse(any(record["args"][0] == "apply" for record in records))

    def test_invalid_plan_json_cannot_reach_apply(self):
        for job, step, _ in self.cases:
            with self.subTest(step=step):
                result, records, applied = self._execute(job, step, "{invalid")
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(any(record["args"][0] == "show" for record in records))
                self.assertIsNone(applied)
                self.assertFalse(any(record["args"][0] == "apply" for record in records))


if __name__ == "__main__":
    unittest.main()
