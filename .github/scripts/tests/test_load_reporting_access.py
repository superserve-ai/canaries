import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "grant-load-reporting-access.sh"


class LoadReportingAccessTest(unittest.TestCase):
    def run_script(self, identity):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            calls = directory / "calls.jsonl"
            terraform = directory / "terraform"
            terraform.write_text(
                "#!/usr/bin/env python3\n"
                "import json,os,sys\n"
                "with open(os.environ['TEST_TERRAFORM_CALLS'],'a') as output:\n"
                " output.write(json.dumps(sys.argv[1:])+'\\n')\n"
            )
            terraform.chmod(0o700)
            environment = dict(os.environ)
            environment["PATH"] = str(directory) + os.pathsep + environment["PATH"]
            environment["TEST_TERRAFORM_CALLS"] = str(calls)
            environment["TF_VAR_deployment_service_account_email"] = identity
            result = subprocess.run(["bash", str(SCRIPT)], env=environment, capture_output=True, text=True)
            recorded = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
            return result, recorded

    def test_preflight_targets_only_the_staging_webhook_permission(self):
        result, calls = self.run_script("deployment@example.invalid")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[0], ["init", "-input=false"])
        self.assertEqual(len(calls), 2)
        apply = calls[1]
        self.assertEqual(apply[0], "apply")
        self.assertEqual(
            [argument for argument in apply if argument.startswith("-target=")],
            ["-target=google_secret_manager_secret_iam_member.staging_load_reporting_accessor"],
        )
        self.assertIn("-var=image=unused-for-load-reporting-access", apply)
        self.assertIn("-var=load_runner_image=unused-for-load-reporting-access", apply)
        self.assertIn("-var=ui_canary_image=unused-for-load-reporting-access", apply)

    def test_missing_production_identity_fails_before_any_terraform_action(self):
        result, calls = self.run_script("")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main()
