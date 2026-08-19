# Superserve API Canaries

This repository deploys independent scheduled API lifecycle canaries for Superserve.

Current targets:
- `staging/us-central1`
- `production/us-central1`
- `production/us-east4`
- `production/us-west2`

Each target has its own:
- Cloud Run Job
- Cloud Scheduler trigger
- runtime service account
- Secret Manager secret reference
- target labels
- target-scoped lock
- independent alert policy

The canary binary supports:
- `lifecycle`
- `janitor`

## Architecture

The lifecycle canary uses the public customer API with `X-API-Key` authentication, not internal service endpoints.

Per run it:
1. acquires a target-scoped lease from GCS
2. creates a sandbox with canary metadata
3. launches from the `superserve/python-3.11` template so `python3` is present in the sandbox
4. verifies command execution
5. verifies pause/resume persistence for disk and memory
6. starts an HTTP server in the sandbox
7. verifies the deterministic public preview URL returns the exact run token
8. deletes the sandbox in a best-effort cleanup path

The janitor:
1. lists canary-owned sandboxes for the environment
2. deletes stale resources past TTL
3. emits orphan and deletion metrics

## Target Inventory

Discovered from `sandbox`:

| Target | Project | Region | API base URL | Preview domain |
| --- | --- | --- | --- | --- |
| staging | `rayai-dev` | `us-central1` | `https://api-staging.superserve.ai` | `staging-sandbox.superserve.ai` |
| production central | `rayai-prod` | `us-central1` | `https://api.superserve.ai` | `sandbox.superserve.ai` |
| production east | `rayai-prod` | `us-east4` | `https://api.superserve.ai` | `use-sandbox.superserve.ai` |
| production west | `rayai-prod` | `us-west2` | `https://usw-api.superserve.ai` | `usw-sandbox.superserve.ai` |

## Required Secrets

Terraform creates secret containers only. Populate versions manually after apply.

Expected secret names:
- `api-canary-key-staging-us-central1`
- `api-canary-key-production-us-central1`
- `api-canary-key-production-us-east4`
- `api-canary-key-production-us-west2`

Each secret value must be a customer API key for a dedicated canary account or team.

Rotate credentials by:
1. create a new API key in Superserve
2. add a new Secret Manager version
3. rerun the Cloud Run Job or wait for the next schedule
4. revoke the old API key

## Local Execution

Local runs use explicit runtime and backend settings. The defaults are:

- `CANARY_RUNTIME=local`
- `CANARY_METRICS_EXPORTER=none`
- `CANARY_LOCK_BACKEND=file`
- `CANARY_LOCK_FILE=/tmp/superserve-canary-<target>.lock`

Examples:

```bash
CANARY_RUNTIME=local \
CANARY_METRICS_EXPORTER=none \
CANARY_LOCK_BACKEND=file \
go run ./cmd/api-canary
```

```bash
CANARY_RUNTIME=local \
CANARY_METRICS_EXPORTER=none \
CANARY_LOCK_BACKEND=none \
go run ./cmd/api-canary
```

```bash
CANARY_RUNTIME=local \
CANARY_METRICS_EXPORTER=stdout \
CANARY_LOCK_BACKEND=file \
go run ./cmd/api-canary
```

```bash
gcloud auth application-default login

CANARY_RUNTIME=local \
CANARY_METRICS_EXPORTER=none \
CANARY_LOCK_BACKEND=gcs \
go run ./cmd/api-canary
```

For lifecycle and janitor runs you still need the scenario-specific inputs:

```bash
export CANARY_MODE=lifecycle
export CANARY_TARGET=staging-us-central1
export CANARY_ENVIRONMENT=staging
export CANARY_REGION=us-central1
export GCP_PROJECT_ID=rayai-dev
export API_BASE_URL=https://api-staging.superserve.ai
export PREVIEW_DOMAIN=staging-sandbox.superserve.ai
export CANARY_API_KEY=ss_live_...
export MANUAL_STAGING_OPT_IN=true

go run ./cmd/api-canary -mode lifecycle
go run ./cmd/api-canary -mode janitor
```

Manual staging execution creates and deletes real resources. Use a dedicated canary API key.

### Load runner

The load runner can also be run directly from a local checkout. It uses the same `CANARY_API_KEY` as the regular canary. Staging is the default deployment target, but the binary is not staging-only: it supports the current target inventory above. Production runs require the explicit `LOAD_TEST_PRODUCTION_OPT_IN=true` guard.

For a minimal staging run, start with one operation and one worker:

```bash
export CANARY_TARGET=staging-us-central1
export CANARY_ENVIRONMENT=staging
export CANARY_REGION=us-central1
export API_BASE_URL=https://api-staging.superserve.ai
export PREVIEW_DOMAIN=staging-sandbox.superserve.ai
export CANARY_API_KEY=ss_live_...
export LOAD_TEST_OPERATIONS=1
export LOAD_TEST_CONCURRENCY=1

go run ./cmd/load-runner
```

A production run uses the same binary and configuration shape, with the target-specific routing values and an explicit production opt-in. For example, `production/us-east4` uses:

```bash
export CANARY_TARGET=production-us-east4
export CANARY_ENVIRONMENT=production
export CANARY_REGION=us-east4
export API_BASE_URL=https://api.superserve.ai
export PREVIEW_DOMAIN=use-sandbox.superserve.ai
export CANARY_API_KEY=ss_live_...
export LOAD_TEST_PRODUCTION_OPT_IN=true
export LOAD_TEST_OPERATIONS=1
export LOAD_TEST_CONCURRENCY=1

go run ./cmd/load-runner
```

`go run` builds the load runner in a temporary location and executes it, so a separate build step or Makefile is not required. To build a reusable local binary instead:

```bash
go build -o ./bin/load-runner ./cmd/load-runner
./bin/load-runner
```

The load runner creates real sandboxes, waits for them to become active, verifies command execution, and deletes them after each operation.

## Debug Retention

Failed sandboxes can be retained for debugging when retention is enabled.

Local example:

```bash
CANARY_RETAIN_FAILED_SANDBOX=true \
CANARY_RETAIN_FAILED_SANDBOX_TTL=4h \
CANARY_RUNTIME=local \
CANARY_METRICS_EXPORTER=none \
CANARY_LOCK_BACKEND=file \
go run ./cmd/api-canary
```

Staging is configured to retain failed sandboxes for 2h. Production keeps retention disabled unless explicitly enabled in Terraform.

When a sandbox is retained:
- the sandbox ID is logged
- the failed step is logged
- the retained sandbox is marked with debug metadata
- the janitor deletes it after the retention TTL expires

For a 2h retention TTL, the janitor should run hourly. A slower cadence can leave retained sandboxes around longer than the logical TTL.

To inspect a retained sandbox:
1. Find the failure log entry with `sandbox_id`
2. Open the sandbox in the Superserve UI or use the API client
3. Check the `failed_step`, `retained_at`, and `expires_at` metadata
4. Inspect `/tmp/verification-utilities` to see the exact scripts the canary uploaded
5. Confirm the expected port is still listening inside the sandbox
6. Delete it manually when finished, or wait for the janitor sweep

Preview failure troubleshooting:
1. Find the failed run in logs
2. Obtain the `sandbox_id` and `failed_step`
3. Confirm the sandbox is still running
4. Check the HTTP server process inside the sandbox
5. Verify the expected port is listening
6. Inspect the preview URL creation response
7. Resolve the preview hostname
8. Request the URL manually
9. Check routing and proxy logs
10. Delete the sandbox manually when finished

Cloud Run jobs require explicit observability and lock settings:

```bash
CANARY_RUNTIME=cloud-run
CANARY_METRICS_EXPORTER=otlp
CANARY_LOCK_BACKEND=gcs
OTEL_EXPORTER_OTLP_ENDPOINT=<existing-collector-endpoint>
LOCK_BUCKET=<gcs-lock-bucket>
```

The staging Cloud Run job explicitly sets:

```text
CANARY_RETAIN_FAILED_SANDBOX=true
CANARY_RETAIN_FAILED_SANDBOX_TTL=2h
```

Production jobs explicitly set:

```text
CANARY_RETAIN_FAILED_SANDBOX=false
```

## Manual Staging Runbook

Use this only after the staging Terraform root has been applied and the dedicated canary API key exists.

Checklist:
1. Confirm the staging job and janitor resources were created.
2. Populate the Secret Manager secret version with the canary API key.
3. Execute the staging lifecycle job once by hand.
4. Inspect the job logs for the created run ID, sandbox ID, and cleanup outcome.
5. If the run fails or leaves a resource behind, execute the janitor job.
6. Retained sandboxes carry `retained_for_debug=true`, `failed_step`, `retained_at`, and `expires_at` metadata.

Exact secret population commands:

```bash
gcloud secrets create api-canary-key-staging-us-central1 \
  --project rayai-dev \
  --replication-policy=automatic

printf '%s' 'ss_live_...' | gcloud secrets versions add api-canary-key-staging-us-central1 \
  --project rayai-dev \
  --data-file=-
```

If the secret already exists, skip `gcloud secrets create` and only add a new version.

Exact staging job commands:

```bash
gcloud run jobs execute api-canary-staging-us-central1 \
  --project rayai-dev \
  --region us-central1 \
  --wait

gcloud run jobs execute api-canary-janitor-staging \
  --project rayai-dev \
  --region us-central1 \
  --wait
```

The staging Terraform root also deploys the single-task load-runner job as `sandbox-load-runner-staging-us-central1`. Running it with no overrides uses the job defaults (`LOAD_TEST_OPERATIONS=100`, `LOAD_TEST_CONCURRENCY=10`):

```bash
gcloud run jobs execute sandbox-load-runner-staging-us-central1 \
  --project rayai-dev \
  --region us-central1 \
  --wait
```

For a specific run size and per-runner lifecycle concurrency, override the environment variables only for that execution:

```bash
gcloud run jobs execute sandbox-load-runner-staging-us-central1 \
  --project rayai-dev \
  --region us-central1 \
  --update-env-vars="LOAD_TEST_OPERATIONS=1000,LOAD_TEST_CONCURRENCY=100" \
  --wait
```

You can optionally supply a stable run ID for easier log correlation:

```bash
gcloud run jobs execute sandbox-load-runner-staging-us-central1 \
  --project rayai-dev \
  --region us-central1 \
  --update-env-vars="LOAD_TEST_OPERATIONS=1000,LOAD_TEST_CONCURRENCY=100,LOAD_TEST_RUN_ID=staging-1000x100" \
  --wait
```

`LOAD_TEST_OPERATIONS` is the global operation count for this single task and `LOAD_TEST_CONCURRENCY` is the maximum number of lifecycle workers in flight inside that task. Multi-task Cloud Run fan-out (`--tasks`, shared `run_id`, unique per-task `worker_id`) is intentionally deferred to SS-346.

Use `gcloud run jobs executions list` and `gcloud run jobs executions describe` to inspect status and logs after each run.

To manually clear retained sandboxes, rerun the janitor job after the retention TTL has elapsed.

## Manual Production Runbook

Production lifecycle canaries are scheduled automatically after the production Terraform roots have been applied and the production API key secrets have been populated.

The load-runner binary supports production targets, but this PR does not require a production load-runner deployment. A production job can be added independently later using the same `/load-runner` binary, the appropriate production secret and routing values, and `LOAD_TEST_PRODUCTION_OPT_IN=true`.

The east production region uses the public Google Telemetry API instead of the private collector path, so it does not need VPC access.

Checklist:
1. Confirm the production lifecycle and janitor jobs were created for all three regions.
2. Populate the production Secret Manager secret versions with the canary API keys.
3. Verify the scheduled lifecycle executions start after the secrets are populated.
4. Inspect the job logs for the created run ID, sandbox ID, and cleanup outcome.
5. If a run fails or leaves a resource behind, execute the matching janitor job.
6. Retained sandboxes carry `retained_for_debug=true`, `failed_step`, `retained_at`, and `expires_at` metadata.

Exact production job commands:

```bash
gcloud run jobs execute api-canary-production-us-central1 \
  --project rayai-prod \
  --region us-central1 \
  --wait

gcloud run jobs execute api-canary-janitor-production-us-central1 \
  --project rayai-prod \
  --region us-central1 \
  --wait

gcloud run jobs execute api-canary-production-us-east4 \
  --project rayai-prod \
  --region us-east4 \
  --wait

gcloud run jobs execute api-canary-janitor-production-us-east4 \
  --project rayai-prod \
  --region us-east4 \
  --wait

gcloud run jobs execute api-canary-production-us-west2 \
  --project rayai-prod \
  --region us-west2 \
  --wait

gcloud run jobs execute api-canary-janitor-production-us-west2 \
  --project rayai-prod \
  --region us-west2 \
  --wait
```

Use `gcloud run jobs executions list` and `gcloud run jobs executions describe` to inspect status and logs after each run.

## Container Build

Build locally:

```bash
docker build -t api-canary:local .
```

## Terraform

Terraform roots:
- [infra/envs/staging/us-central1](/home/lando/superserve-ai/canaries/infra/envs/staging/us-central1/main.tf:1)
- [infra/envs/production](/home/lando/superserve-ai/canaries/infra/envs/production/main.tf:1)
- [infra/envs/production/us-east4](/home/lando/superserve-ai/canaries/infra/envs/production/us-east4/main.tf:1)

Backends follow the discovered sandbox convention:
- staging bucket: `superserve-terraform-state`, prefix `canaries/staging/us-central1`
- production bucket: `superserve-terraform-state-prod`, prefix `canaries/production`
- production east bucket prefix: `canaries/production/us-east4`

Apply order:
1. apply staging
2. populate staging secret
3. manually run and validate staging job
4. apply production
5. apply production/us-east4
6. populate production secrets before the first scheduled run window
7. verify all production jobs run successfully after scheduling starts

Cloud Run jobs set `CANARY_RUNTIME=cloud-run`, `CANARY_METRICS_EXPORTER=otlp`, and `CANARY_LOCK_BACKEND=gcs` explicitly, and pass the OTLP endpoint through Terraform locals instead of relying on application defaults. The deployment uses the standard `OTEL_EXPORTER_OTLP_ENDPOINT` path that the sandbox stack already uses.

Production east4 points `OTEL_EXPORTER_OTLP_ENDPOINT` at `https://telemetry.googleapis.com` and grants the runtime service accounts `roles/monitoring.metricWriter` plus `roles/serviceusage.serviceUsageConsumer` so metrics can be exported without VPC connectivity.

## Metrics And Alerts

Metrics are emitted over OTLP HTTP and intended to feed the existing GMP path.

Alerting behavior:
- staging enables the full canary alert set
- production only creates the consecutive-failure lifecycle alert
- the GitHub deploy service account must be granted the alert-management IAM roles in Terraform; the deploy workflow passes its service account email to Terraform as `deployment_service_account_email`
- notification channel IDs are not managed here; pass the existing GMP/Slack channel resource names as a Terraform list literal via `TF_VAR_NOTIFICATION_CHANNEL_IDS`, for example `["projects/rayai-prod/notificationChannels/123456789"]`

Runtime selectors:
- `CANARY_RUNTIME=local`
- `CANARY_RUNTIME=cloud-run`
- `CANARY_METRICS_EXPORTER=none|stdout|otlp`
- `CANARY_LOCK_BACKEND=none|file|gcs`

Primary metrics:
- `superserve_canary_run_total`
- `superserve_canary_run_duration_seconds`
- `superserve_canary_step_total`
- `superserve_canary_step_duration_seconds`
- `superserve_canary_cleanup_total`
- `superserve_canary_orphan_resources`
- `superserve_canary_overlap_skipped_total`
- `superserve_canary_last_success_timestamp_seconds`

Bounded labels:
- `environment`
- `region`
- `target`
- `scenario`
- `step`
- `result`

## Failure Investigation

Runbook:
1. Identify the target and failed step from the alert.
2. Find Cloud Run Job logs filtered by target and run ID.
3. Confirm whether cleanup succeeded.
4. If cleanup failed, inspect the remaining sandbox using the canary account.
5. Run the janitor job manually if stale resources remain.
6. Rotate the API key if authentication failures repeat.
7. Re-run the Cloud Run Job manually.

## Future Work

The current implementation uses deterministic public preview URLs because no separate public “expose port” control-plane API was found in the inspected `sandbox` repository. The preview verification path is isolated so future authenticated preview access can replace the direct GET without rewriting the lifecycle orchestration.
