"""Catch environment wiring gaps before removing project-wide deployment actAs."""

from pathlib import Path
import re
import unittest


INFRA = Path(__file__).resolve().parents[3]
ROOTS = (
    INFRA / "envs/production",
    INFRA / "envs/production/us-east4",
    INFRA / "envs/staging/us-central1",
)
IDENTITY_MODULES = {"canary_target", "janitor", "ui_canary"}


def blocks(source, kind):
    pattern = rf'(?ms)^{kind} "([^"\n]+)"(?: "([^"\n]+)")? \{{(.*?)^\}}'
    return [(first, second, body) for first, second, body in re.findall(pattern, source)]


class DeploymentAccessWiringTest(unittest.TestCase):
    def test_every_identity_module_receives_the_deployment_principal(self):
        for root in ROOTS:
            checked = set()
            for name, _, body in blocks((root / "main.tf").read_text(), "module"):
                source = re.search(r'source\s*=\s*"([^"\n]+)"', body)
                if source is None or Path(source.group(1)).name not in IDENTITY_MODULES:
                    continue
                with self.subTest(root=root, module=name):
                    self.assertRegex(
                        body,
                        r"deployment_service_account_email\s*=\s*var\.deployment_service_account_email",
                    )
                checked.add(name)
            self.assertIn("lifecycle", checked)
            self.assertIn("janitor", checked)
            self.assertIn("ui_canary", checked)

    def test_staging_load_runner_has_account_scoped_deployment_access(self):
        source = (ROOTS[2] / "main.tf").read_text()
        resources = {(kind, name): body for kind, name, body in blocks(source, "resource")}
        grant = resources[("google_service_account_iam_member", "load_runner_deploy_act_as")]
        self.assertRegex(grant, r"service_account_id\s*=\s*google_service_account\.load_runner\.name")
        self.assertRegex(grant, r'role\s*=\s*"roles/iam.serviceAccountUser"')
        self.assertIn('"serviceAccount:${var.deployment_service_account_email}"', grant)
        job = resources[("google_cloud_run_v2_job", "load_runner")]
        self.assertRegex(
            job,
            r"(?s)depends_on\s*=\s*\[.*?google_service_account_iam_member\.load_runner_deploy_act_as.*?\]",
        )


if __name__ == "__main__":
    unittest.main()
