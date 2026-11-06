#!/usr/bin/env python3
"""Wait for effective deployment access to a newly granted service account."""

import argparse
import json
import math
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


PERMISSION = "iam.serviceAccounts.actAs"
_ACCOUNT = r"(?:[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+\.gserviceaccount\.com|[0-9]+)"
_RESOURCE = re.compile(r"projects/(?:-|[A-Za-z0-9_-]+)/serviceAccounts/" + _ACCOUNT)
_EMAIL = re.compile(r"[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+\.gserviceaccount\.com")
_PROVIDER_OVERRIDES = (
    "GOOGLE_CREDENTIALS",
    "GOOGLE_CLOUD_KEYFILE_JSON",
    "GCLOUD_KEYFILE_JSON",
    "GOOGLE_IMPERSONATE_SERVICE_ACCOUNT",
)


class ReadinessError(RuntimeError):
    """A sanitized configuration or permission-check failure."""


class _Pending(Exception):
    pass


def service_account_resource(value):
    if isinstance(value, str) and _EMAIL.fullmatch(value):
        return "projects/-/serviceAccounts/" + value
    if not isinstance(value, str) or not _RESOURCE.fullmatch(value):
        raise ValueError("service account must be an email or projects/PROJECT/serviceAccounts/ACCOUNT")
    return value


def positive_seconds(value):
    try:
        seconds = float(value)
    except (TypeError, ValueError, OverflowError):
        raise ValueError("timeout must be a finite positive number") from None
    if not math.isfinite(seconds) or seconds <= 0:
        raise ValueError("timeout must be a finite positive number")
    return seconds


def _token(value):
    if not isinstance(value, str):
        raise ReadinessError("deployment credentials did not contain a valid access token")
    token = value.strip()
    if not token or any(char.isspace() for char in token):
        raise ReadinessError("deployment credentials did not contain a valid access token")
    return token


def access_token(environ, timeout):
    if environ.get("GOOGLE_IMPERSONATE_SERVICE_ACCOUNT"):
        raise ReadinessError(
            "provider impersonation is unsupported; supply credentials for the deployment identity directly"
        )
    if environ.get("GOOGLE_OAUTH_ACCESS_TOKEN"):
        return _token(environ["GOOGLE_OAUTH_ACCESS_TOKEN"])
    if any(environ.get(name) for name in _PROVIDER_OVERRIDES):
        raise ReadinessError(
            "provider credential overrides are unsupported; supply a matching OAuth access token "
            "or use the deployment ADC credential file without other provider overrides"
        )

    environment = dict(environ)
    credential_file = environ.get("GOOGLE_APPLICATION_CREDENTIALS")
    if credential_file:
        environment["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"] = credential_file
        command = ["gcloud", "auth", "print-access-token"]
    else:
        environment.pop("CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", None)
        command = ["gcloud", "auth", "application-default", "print-access-token"]
    # Terraform's ADC source must not acquire a second impersonation layer
    # from unrelated Cloud SDK user configuration.
    environment["CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT"] = ""
    environment["CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"] = ""
    environment["CLOUDSDK_AUTH_ACCESS_TOKEN"] = ""
    try:
        result = subprocess.run(
            command, env=environment, capture_output=True, text=True, timeout=timeout
        )
    except subprocess.TimeoutExpired:
        raise _Pending() from None
    except OSError:
        raise ReadinessError("cannot run Google Cloud CLI to obtain deployment credentials") from None
    if result.returncode:
        raise ReadinessError("cannot obtain deployment credentials from Google Cloud CLI")
    return _token(result.stdout)


def has_act_as(resource, token, timeout):
    encoded = urllib.parse.quote(resource, safe="/@._-")
    request = urllib.request.Request(
        "https://iam.googleapis.com/v1/" + encoded + ":testIamPermissions",
        data=json.dumps({"permissions": [PERMISSION]}).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            value = json.load(response)
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()
        if status in (403, 404, 429) or 500 <= status < 600:
            return False
        raise ReadinessError("IAM permission check failed with HTTP " + str(status)) from None
    except (urllib.error.URLError, TimeoutError, ConnectionError):
        return False
    except (ValueError, UnicodeError):
        raise ReadinessError("IAM permission check returned invalid JSON") from None
    if not isinstance(value, dict):
        raise ReadinessError("IAM permission check returned an invalid response")
    permissions = value.get("permissions", [])
    if not isinstance(permissions, list) or not all(isinstance(item, str) for item in permissions):
        raise ReadinessError("IAM permission check returned invalid permissions")
    return PERMISSION in permissions


def wait_for_act_as(service_account, timeout=600, environ=None):
    resource = service_account_resource(service_account)
    timeout = positive_seconds(timeout)
    environment = os.environ if environ is None else environ
    deadline = time.monotonic() + timeout
    announced = False
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("deployment actAs permission did not become effective for " + resource)
        try:
            token = access_token(environment, min(30, remaining))
            remaining = deadline - time.monotonic()
            if remaining > 0 and has_act_as(resource, token, min(20, remaining)):
                print("Deployment actAs permission is ready for " + resource, flush=True)
                return
        except _Pending:
            pass
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("deployment actAs permission did not become effective for " + resource)
        if not announced:
            print("Waiting for deployment actAs permission on " + resource, flush=True)
            announced = True
        time.sleep(min(10, remaining))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--service-account", required=True)
    parser.add_argument("--timeout", default=600, type=positive_seconds)
    args = parser.parse_args(argv)
    try:
        wait_for_act_as(args.service_account, args.timeout)
    except (ReadinessError, TimeoutError, ValueError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
