#!/usr/bin/env bash
set -euo pipefail

: "${TF_VAR_deployment_service_account_email:?production deployment identity is required}"

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root/infra/envs/production"
terraform init -input=false

# The existing staging identity needs access before Cloud Run validates its
# cross-project secret. This phase must not deploy production applications.
terraform apply \
  -input=false \
  -auto-approve \
  -target=google_secret_manager_secret_iam_member.staging_load_reporting_accessor \
  -var=image=unused-for-load-reporting-access \
  -var=load_runner_image=unused-for-load-reporting-access \
  -var=ui_canary_image=unused-for-load-reporting-access
