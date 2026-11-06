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
            r"(?s)depends_on\s*=\s*\[.*?terraform_data\.load_runner_deploy_act_as_ready.*?\]",
        )


    def test_consumers_wait_for_effective_permission_and_account_replacement(self):
        for module, job in (("canary_target", "lifecycle"), ("janitor", "janitor"), ("ui_canary", "ui_canary")):
            source = (INFRA / "modules" / module / "main.tf").read_text()
            resources = {(kind, name): body for kind, name, body in blocks(source, "resource")}
            for identity, consumer in (("runtime", "google_cloud_run_v2_job"), ("scheduler", "google_cloud_scheduler_job")):
                with self.subTest(module=module, identity=identity):
                    readiness = resources[("terraform_data", identity + "_deploy_act_as_ready")]
                    self.assertIn(f"google_service_account.{identity}.unique_id", readiness)
                    self.assertIn(f"google_service_account_iam_member.{identity}_deploy_act_as[0].id", readiness)
                    self.assertIn("wait_act_as.py", readiness)
                    self.assertIn(f"TF_ACT_AS_SERVICE_ACCOUNT = google_service_account.{identity}.name", readiness)
                    body = resources[(consumer, job)]
                    self.assertRegex(body, rf"(?s)depends_on\s*=\s*\[.*?terraform_data\.{identity}_deploy_act_as_ready.*?\]")

        staging = (ROOTS[2] / "main.tf").read_text()
        resources = {(kind, name): body for kind, name, body in blocks(staging, "resource")}
        readiness = resources[("terraform_data", "load_runner_deploy_act_as_ready")]
        self.assertIn("google_service_account.load_runner.unique_id", readiness)
        self.assertIn("google_service_account_iam_member.load_runner_deploy_act_as.id", readiness)
        self.assertIn("wait_act_as.py", readiness)


if __name__ == "__main__":
    unittest.main()
