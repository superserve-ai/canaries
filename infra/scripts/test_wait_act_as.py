"""Exercise delayed IAM activation without cloud calls or real sleeps."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import Mock, patch
import urllib.error


SPEC = importlib.util.spec_from_file_location("wait_act_as", Path(__file__).with_name("wait_act_as.py"))
wait = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(wait)

RESOURCE = "projects/example-project/serviceAccounts/runtime@example-project.iam.gserviceaccount.com"
SECRET = "example-secret-access-token"


def response(value):
    return io.BytesIO(json.dumps(value).encode())


def http_error(status):
    return urllib.error.HTTPError(
        "https://iam.googleapis.com/example", status, "example error", None,
        io.BytesIO(("private body " + SECRET).encode()),
    )


class Clock:
    def __init__(self):
        self.now = 0
        self.sleeps = []

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class WaitActAsTests(unittest.TestCase):
    def test_delayed_effective_permission_waits_through_empty_and_denied_responses(self):
        clock = Clock()
        output = io.StringIO()
        replies = [response({}), http_error(403), response({"permissions": [wait.PERMISSION]})]
        with patch.object(wait.time, "monotonic", clock.monotonic), \
                patch.object(wait.time, "sleep", clock.sleep), \
                patch.object(wait.urllib.request, "urlopen", side_effect=replies) as request, \
                contextlib.redirect_stdout(output):
            wait.wait_for_act_as(RESOURCE, timeout=60, environ={"GOOGLE_OAUTH_ACCESS_TOKEN": SECRET})
        self.assertEqual(request.call_count, 3)
        self.assertEqual(clock.sleeps, [10, 10])
        self.assertEqual(output.getvalue().count("Waiting for"), 1)
        self.assertEqual(output.getvalue().count("is ready"), 1)
        self.assertNotIn(SECRET, output.getvalue())
        for call in request.call_args_list:
            req = call.args[0]
            self.assertEqual(req.method, "POST")
            self.assertEqual(req.full_url, "https://iam.googleapis.com/v1/" + RESOURCE + ":testIamPermissions")
            self.assertEqual(json.loads(req.data), {"permissions": [wait.PERMISSION]})
            self.assertEqual(req.get_header("Authorization"), "Bearer " + SECRET)
            self.assertNotIn(SECRET, req.full_url)

    def test_permanent_denial_has_a_bounded_deadline(self):
        clock = Clock()
        with patch.object(wait.time, "monotonic", clock.monotonic), \
                patch.object(wait.time, "sleep", clock.sleep), \
                patch.object(wait.urllib.request, "urlopen", side_effect=lambda *a, **k: response({})) as request, \
                contextlib.redirect_stdout(io.StringIO()), self.assertRaises(TimeoutError) as failure:
            wait.wait_for_act_as(RESOURCE, timeout=25, environ={"GOOGLE_OAUTH_ACCESS_TOKEN": SECRET})
        self.assertEqual(clock.now, 25)
        self.assertEqual(clock.sleeps, [10, 10, 5])
        self.assertEqual([c.kwargs["timeout"] for c in request.call_args_list], [20, 15, 5])
        self.assertNotIn(SECRET, str(failure.exception))

    def test_transient_iam_and_network_failures_retry(self):
        failures = [http_error(status) for status in (403, 404, 429, 500, 503, 599)]
        failures += [urllib.error.URLError("private " + SECRET), TimeoutError(SECRET), ConnectionResetError(SECRET)]
        for failure in failures:
            with self.subTest(failure=type(failure).__name__), \
                    patch.object(wait.urllib.request, "urlopen", side_effect=failure):
                self.assertFalse(wait.has_act_as(RESOURCE, SECRET, 1))

    def test_other_http_errors_fail_without_exposing_the_response(self):
        for status in (400, 401, 405):
            with self.subTest(status=status), \
                    patch.object(wait.urllib.request, "urlopen", side_effect=http_error(status)), \
                    self.assertRaises(wait.ReadinessError) as failure:
                wait.has_act_as(RESOURCE, SECRET, 1)
            self.assertEqual(str(failure.exception), "IAM permission check failed with HTTP " + str(status))
            self.assertNotIn(SECRET, str(failure.exception))

    def test_malformed_responses_cannot_be_mistaken_for_granted_permission(self):
        replies = [
            io.BytesIO(("invalid " + SECRET).encode()),
            response([wait.PERMISSION]),
            response({"permissions": wait.PERMISSION}),
            response({"permissions": None}),
            response({"permissions": [wait.PERMISSION, 1]}),
        ]
        for reply in replies:
            with self.subTest(reply=reply), patch.object(wait.urllib.request, "urlopen", return_value=reply), \
                    self.assertRaises(wait.ReadinessError) as failure:
                wait.has_act_as(RESOURCE, SECRET, 1)
            self.assertNotIn(SECRET, str(failure.exception))

    def test_only_the_requested_effective_permission_satisfies_readiness(self):
        for value in ({}, {"permissions": []}, {"permissions": ["iam.serviceAccounts.getIamPolicy"]}):
            with patch.object(wait.urllib.request, "urlopen", return_value=response(value)):
                self.assertFalse(wait.has_act_as(RESOURCE, SECRET, 1))

    def test_explicit_provider_access_token_takes_precedence(self):
        environment = {"GOOGLE_OAUTH_ACCESS_TOKEN": SECRET, "GOOGLE_CREDENTIALS": "private credentials"}
        with patch.object(wait.subprocess, "run") as command:
            self.assertEqual(wait.access_token(environment, 5), SECRET)
        command.assert_not_called()

    def test_provider_impersonation_rejects_even_with_an_explicit_access_token(self):
        environment = {
            "GOOGLE_OAUTH_ACCESS_TOKEN": SECRET,
            "GOOGLE_IMPERSONATE_SERVICE_ACCOUNT": "deployment@example-project.iam.gserviceaccount.com",
        }
        with patch.object(wait.subprocess, "run") as command, \
                self.assertRaises(wait.ReadinessError) as failure:
            wait.access_token(environment, 5)
        command.assert_not_called()
        self.assertNotIn(SECRET, str(failure.exception))

    def test_provider_overrides_reject_even_with_an_adc_file(self):
        for name in wait._PROVIDER_OVERRIDES:
            environment = {name: SECRET, "GOOGLE_APPLICATION_CREDENTIALS": "/private/adc.json"}
            with self.subTest(name=name), patch.object(wait.subprocess, "run") as command, \
                    self.assertRaises(wait.ReadinessError) as failure:
                wait.access_token(environment, 5)
            command.assert_not_called()
            self.assertNotIn(SECRET, str(failure.exception))

    def test_adc_file_overrides_unrelated_cloud_sdk_credentials(self):
        environment = {
            "GOOGLE_APPLICATION_CREDENTIALS": "/private/deployment-adc.json",
            "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE": "/private/unrelated.json",
            "CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "unrelated@example-project.iam.gserviceaccount.com",
        }
        with patch.object(wait.subprocess, "run", return_value=Mock(returncode=0, stdout=SECRET + "\n")) as command:
            self.assertEqual(wait.access_token(environment, 7), SECRET)
        self.assertEqual(command.call_args.args[0], ["gcloud", "auth", "print-access-token"])
        self.assertEqual(command.call_args.kwargs["env"]["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"], environment["GOOGLE_APPLICATION_CREDENTIALS"])
        self.assertEqual(command.call_args.kwargs["env"]["CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT"], "")
        self.assertEqual(command.call_args.kwargs["timeout"], 7)
        self.assertEqual(environment["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"], "/private/unrelated.json")
        self.assertNotIn(SECRET, " ".join(command.call_args.args[0]))

    def test_sdk_only_override_cannot_replace_terraform_application_default_identity(self):
        environment = {"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE": "/private/unrelated.json"}
        with patch.object(wait.subprocess, "run", return_value=Mock(returncode=0, stdout=SECRET)) as command:
            self.assertEqual(wait.access_token(environment, 5), SECRET)
        self.assertEqual(command.call_args.args[0], ["gcloud", "auth", "application-default", "print-access-token"])
        self.assertNotIn("CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", command.call_args.kwargs["env"])
        self.assertEqual(environment["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"], "/private/unrelated.json")

    def test_local_fallback_uses_application_default_identity(self):
        with patch.object(wait.subprocess, "run", return_value=Mock(returncode=0, stdout=SECRET)) as command:
            self.assertEqual(wait.access_token({}, 5), SECRET)
        self.assertEqual(command.call_args.args[0], ["gcloud", "auth", "application-default", "print-access-token"])

    def test_sdk_only_access_tokens_cannot_override_the_provider_adc_source(self):
        environment = {
            "GOOGLE_APPLICATION_CREDENTIALS": "/private/deployment-adc.json",
            "CLOUDSDK_AUTH_ACCESS_TOKEN": "unrelated-" + SECRET,
            "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE": "/private/unrelated-token.txt",
        }
        output, error = io.StringIO(), io.StringIO()
        with patch.object(wait.subprocess, "run", return_value=Mock(returncode=0, stdout=SECRET)) as command, \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(error):
            self.assertEqual(wait.access_token(environment, 5), SECRET)
        child = command.call_args.kwargs["env"]
        self.assertEqual(child["CLOUDSDK_AUTH_ACCESS_TOKEN"], "")
        self.assertEqual(child["CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"], "")
        self.assertEqual(child["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"], environment["GOOGLE_APPLICATION_CREDENTIALS"])
        self.assertEqual(environment["CLOUDSDK_AUTH_ACCESS_TOKEN"], "unrelated-" + SECRET)
        self.assertNotIn(SECRET, " ".join(command.call_args.args[0]))
        self.assertNotIn(SECRET, output.getvalue() + error.getvalue())

    def test_sdk_failure_does_not_disclose_credential_output(self):
        result = Mock(returncode=1, stdout=SECRET, stderr="private " + SECRET)
        with patch.object(wait.subprocess, "run", return_value=result), self.assertRaises(wait.ReadinessError) as failure:
            wait.access_token({}, 5)
        self.assertNotIn(SECRET, str(failure.exception))
        with patch.object(wait.subprocess, "run", side_effect=FileNotFoundError(SECRET)), \
                self.assertRaises(wait.ReadinessError) as failure:
            wait.access_token({}, 5)
        self.assertNotIn(SECRET, str(failure.exception))

    def test_sdk_timeout_consumes_the_same_overall_deadline(self):
        clock = Clock()

        def timed_out(*args, **kwargs):
            clock.now += kwargs["timeout"]
            raise subprocess.TimeoutExpired(args[0], kwargs["timeout"], output=SECRET, stderr=SECRET)

        with patch.object(wait.time, "monotonic", clock.monotonic), \
                patch.object(wait.time, "sleep", clock.sleep), \
                patch.object(wait.subprocess, "run", side_effect=timed_out) as command, \
                patch.object(wait.urllib.request, "urlopen") as request, self.assertRaises(TimeoutError):
            wait.wait_for_act_as(RESOURCE, timeout=12, environ={})
        self.assertEqual(command.call_args.kwargs["timeout"], 12)
        self.assertEqual(clock.now, 12)
        self.assertEqual(clock.sleeps, [])
        request.assert_not_called()

    def test_request_timeout_uses_time_remaining_after_credentials(self):
        clock = Clock()

        def token(*args):
            clock.now += 9
            return SECRET

        with patch.object(wait.time, "monotonic", clock.monotonic), \
                patch.object(wait.time, "sleep", clock.sleep), \
                patch.object(wait, "access_token", side_effect=token), \
                patch.object(wait.urllib.request, "urlopen", return_value=response({"permissions": [wait.PERMISSION]})) as request, \
                contextlib.redirect_stdout(io.StringIO()):
            wait.wait_for_act_as(RESOURCE, timeout=10, environ={})
        self.assertEqual(request.call_args.kwargs["timeout"], 1)
        self.assertEqual(clock.sleeps, [])

    def test_invalid_tokens_are_rejected_without_their_contents(self):
        for token in (" ", SECRET + " another", SECRET + "\nsecond-line"):
            with self.subTest(token=token), self.assertRaises(wait.ReadinessError) as failure:
                wait.access_token({"GOOGLE_OAUTH_ACCESS_TOKEN": token}, 5)
            self.assertNotIn(SECRET, str(failure.exception))

    def test_resource_validation_accepts_only_safe_account_targets(self):
        email = "runtime@example-project.iam.gserviceaccount.com"
        self.assertEqual(wait.service_account_resource(email), "projects/-/serviceAccounts/" + email)
        self.assertEqual(wait.service_account_resource(RESOURCE), RESOURCE)
        self.assertEqual(wait.service_account_resource("projects/123/serviceAccounts/456"), "projects/123/serviceAccounts/456")
        for value in ("", "user@example.com", RESOURCE + "?token=" + SECRET, RESOURCE + "/keys/x", "../account", RESOURCE + "\n"):
            with self.subTest(value=value), self.assertRaises(ValueError) as failure:
                wait.service_account_resource(value)
            self.assertNotIn(SECRET, str(failure.exception))

    def test_invalid_timeouts_fail_before_credentials_or_network(self):
        for timeout in (0, -1, "invalid", float("nan"), float("inf"), -float("inf")):
            with self.subTest(timeout=timeout), patch.object(wait, "access_token") as token, \
                    patch.object(wait.urllib.request, "urlopen") as request, self.assertRaises(ValueError):
                wait.wait_for_act_as(RESOURCE, timeout=timeout, environ={})
            token.assert_not_called()
            request.assert_not_called()

    def test_cli_sanitizes_failure_and_returns_nonzero(self):
        output, error = io.StringIO(), io.StringIO()
        with patch.dict(wait.os.environ, {"GOOGLE_OAUTH_ACCESS_TOKEN": SECRET}, clear=True), \
                patch.object(wait.urllib.request, "urlopen", side_effect=http_error(400)), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(error):
            result = wait.main(["--service-account", RESOURCE, "--timeout", "1"])
        self.assertEqual(result, 1)
        self.assertIn("HTTP 400", error.getvalue())
        self.assertNotIn(SECRET, output.getvalue() + error.getvalue())


if __name__ == "__main__":
    unittest.main()
