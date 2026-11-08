# Superserve API Canaries

This repository deploys independent scheduled API lifecycle canaries for Superserve.

Current targets:
- `staging/us-central1`
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
- `lifecycle` (API lifecycle canary)
- `template`
- `snapshot`
- `burst`
- `dax`
- `soak`
- `janitor` (API orphan cleanup)
- `ui-lifecycle` (Headless browser UI lifecycle canary)

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

The template canary runs hourly as its own Cloud Run Job with its own lock and alerts. Per run it:
1. creates a template whose single build step writes the run token to `/opt/canary/build-token`
2. polls the build until it is ready, failed, or cancelled
3. creates a sandbox from the template and reads the token back, which proves the build steps ran
4. deletes the sandbox, then the template

Templates carry no metadata, so canary templates are named `api-canary-template-<target>-<run_id>` and the janitor sweeps that prefix. The base image is `CANARY_TEMPLATE_BASE_IMAGE` (default `ubuntu:22.04`).

The snapshot canary runs every 15 minutes as its own Cloud Run Job with its own lock and alerts. Per run it:
1. creates a source sandbox and seeds the same disk and memory tokens the lifecycle run uses
2. takes a `mem+fs` snapshot
3. records the snapshot id in the source's metadata as soon as the snapshot exists, polls it to ready, then checks the source is active again
4. creates a fork from the snapshot and runs the disk and memory checks inside the fork
5. deletes the fork, the snapshot, then the source

Snapshots carry no metadata and outlive their sandbox, so the source keeps pointing at its snapshot until the snapshot is gone. A failed snapshot delete leaves the source in place, and the janitor deletes the snapshot before any stale sandbox that still names one.

The `burst` and `dax` scenarios run the public [ComputeSDK benchmarks](https://github.com/computesdk/benchmarks) daily at 12:00 and 12:30 Pacific on each production target and post the result to Slack through the webhook in the `canary-benchmark-slack-webhook` secret (Terraform creates the container; add the webhook URL as a version).

- `burst` creates `CANARY_BURST_SIZE` (default 100) sandboxes from `superserve/node-22` at once and times each from the create request until `node -v` succeeds. The score uses the benchmark's formula: trim the fastest and slowest 5%, score median/p95/p99 against a 10s ceiling with weights 0.60/0.25/0.15, and multiply by the success rate. The run fails if any sandbox fails.
- `dax` runs the benchmark's own build script (vendored in `internal/lifecycle/benchmarks/`) in a sandbox from the 8 vCPU / 16 GiB `benchmark-dax-node22-8cpu-16gb` template, which the first run builds and later runs reuse. It reports each build phase: prepare, cache clear, Bun download and unpack, clone, install, typecheck.

Numbers run lower than the public leaderboard for `burst`, since the job calls the API from inside Google Cloud rather than from the benchmark's runners. The canary team needs room for the burst (`max_sandboxes` above the burst size plus the other scenarios) and an 8 vCPU / 16 GiB template limit for `dax`.

The `soak` scenario runs separately on the production east and west targets twice daily at 00:00 and 12:00 Pacific. It keeps one sandbox for six hours and checks it about once a minute. Its server generates an identity at startup so later checks can prove the original process survived. Setup validates one explicit pause and resume, and a 60-second idle timeout allows automatic pauses later. Each minute the job observes the sandbox status, activates it, and checks the original process. The same server listens on two ports so one authenticated preview stays published while a second preview is created, checked, and deleted each cycle. Each cycle also runs a short command. Failed checks remain visible while the remaining cycles continue; process identity is unknown when a check cannot reach the server. The summary distinguishes the explicit pause/resume check from observed automatic pauses.

Soak runs use the same binary, metrics, per-scenario lock, and Slack webhook as the other benchmark canaries. Only the final summary is posted to Slack, with successes/attempts, success percentages, and colored circles for each operation, skipped checks reported separately, process continuity, and cleanup outcome. Per-check results remain in logs and the existing `superserve_canary_step_total` and `superserve_canary_step_duration_seconds` metrics with `scenario="soak"`. Attempt totals count successes and failures; skipped checks remain separate. Missed scheduled intervals are reported and fail the run, so slow checks cannot silently reduce coverage.

`CANARY_SOAK_DURATION` defaults to `6h` and `CANARY_SOAK_INTERVAL` defaults to `1m`. The scheduled job allows 6h15m for setup and the run, with a 6h20m Cloud Run task timeout and a 6h30m lock. Its sandbox expiry covers the run and cleanup grace period, so the janitor leaves it alone while the run is active and reclaims abandoned resources after expiry. The soak job deletes its sandbox on both success and failure and never retains it for debugging.

The janitor:
1. lists canary-owned sandboxes and canary-named templates for the environment
2. deletes stale resources past TTL, snapshot before the sandbox that names it
3. emits orphan and deletion metrics

## UI Canary

The UI canary runs automated end-to-end browser journeys against the Superserve web console using Playwright in headless Chromium.

### UI Lifecycle Scenario
Per run it:
1. **Authenticates**: Submits operator email and password on `/auth/signin`, verifies session cookie creation and navigates to `/sandboxes/`.
2. **Creates Sandbox**: Uses a unique run name (`ui-canary-<run-id>`) and fills ownership metadata in Advanced Options before submitting. A safety timeout of the run timeout plus five minutes bounds compute, and auto-delete removes sandboxes after five continuous minutes paused. Missing safety controls abort before creation. Regional API tagging verifies ownership after ID discovery, then the UI must report **Active**.
3. **Interactive Terminal Execution**: Opens the web terminal (xterm.js), evaluates a dynamic arithmetic expression in bash (`echo "RES_UI_$((<nonceA> + <nonceB>))"` with random 4-digit nonces), and verifies the computed sum (`RES_UI_<sum>`) to eliminate false positives from keystroke echoing or hardcoded outputs.
4. **Pauses Sandbox**: Clicks Stop and asserts the **Paused** status badge.
5. **Resumes Sandbox**: Clicks Start and asserts the **Active** status badge.
6. **Deletes Sandbox**: Confirms deletion dialog, waits for the dialog to dismiss and verifies the sandbox is removed from the sandboxes table. Deferred cleanup attempts UI deletion on intermediate failures, then uses the regional API to recover sandboxes matching both the exact name and durable run metadata. If the browser never discovers an ID, API recovery still runs. Creation-time tags let the janitor find late or failed recoveries; the safety timeout also bounds abandoned sandbox runtime.

### UI Canary Environment Variables

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `CANARY_UI_CONSOLE_URL` | **Yes** | — | Target Superserve Console URL (e.g. `https://console-staging.superserve.ai` or `http://localhost:3000`) |
| `CANARY_UI_EMAIL` | **Yes** | — | Canary operator login email |
| `CANARY_UI_PASSWORD` | **Yes** | — | Canary operator login password |
| `CANARY_UI_VERCEL_PROTECTION_BYPASS` | No | — | Vercel deployment protection bypass secret (injected via `x-vercel-protection-bypass` and `x-vercel-set-bypass-cookie: true` headers; never embed in `CANARY_UI_CONSOLE_URL`) |
| `CANARY_UI_SANDBOX_ID_PREFIX` | No | — | Expected regional ID prefix (production west: `sb-usw-`, east: `sb-use-`); legacy UUIDs require ownership tagging through the regional API |
| `CANARY_UI_TEAM_LABEL` | No | — | Exact team option label in the Console, including any region suffix. Deployed jobs select the shared API-canary team and fail before creation if the label is missing or ambiguous. |
| `CANARY_UI_HEADLESS` | No | `true` | Run browser in headless mode (`true` or `false`) |
| `CANARY_UI_STEP_TIMEOUT` | No | `45s` | Timeout for UI navigation and status assertion steps |
| `CANARY_UI_TERMINAL_TIMEOUT` | No | `30s` | Timeout for terminal connection and command execution |
| `CANARY_UI_ARTIFACTS_DIR` | No | `/tmp/ui-canary-artifacts` | Directory for failure screenshots |

### Local UI Canary Execution

Run with `make run-ui` (reads credentials and console URL from `.env` or environment):

```bash
export CANARY_UI_CONSOLE_URL=https://console-staging.superserve.ai
export CANARY_UI_VERCEL_PROTECTION_BYPASS=secret123 # required for protected staging
export CANARY_UI_EMAIL=canary@superserve.ai
export CANARY_UI_PASSWORD=secret123

make run-ui
```

### Container Build & Docker Execution

Build the standalone UI Canary image (includes Playwright Chromium runtime and Go driver cache):

```bash
make docker-build-ui

docker run --rm \
  -e CANARY_TARGET=staging-us-central1 \
  -e CANARY_ENVIRONMENT=staging \
  -e CANARY_REGION=us-central1 \
  -e CANARY_UI_CONSOLE_URL=https://console-staging.superserve.ai \
  -e CANARY_UI_VERCEL_PROTECTION_BYPASS=secret123 \
  -e CANARY_UI_EMAIL=canary@superserve.ai \
  -e CANARY_UI_PASSWORD=secret123 \
  superserve/ui-canary:latest
```

### Cloud Run deployment

The UI canary uses a dedicated Playwright image and runs the full browser lifecycle every five minutes once its scheduler is enabled. Deployment builds the image once and publishes it to both projects. Every Terraform plan and apply passes `ui_canary_image` separately from the API image.

| Target | Terraform root | Console | Expected sandbox IDs |
| --- | --- | --- | --- |
| `staging-us-central1` | `infra/envs/staging/us-central1` | `https://console-staging.superserve.ai` | Staging account |
| `production-us-west2` | `infra/envs/production` | `https://console.superserve.ai` | `sb-usw-…` |
| `production-us-east4` | `infra/envs/production/us-east4` | `https://console.superserve.ai` | `sb-use-…` |

Each job has its own runtime identity, GCS lease, login secrets, and regional API key for ownership tagging and janitor cleanup. Production does not bind a Vercel bypass secret. UI alert policies are a follow-up.

#### First deployment

1. For a fresh target, deploy with `ui_scheduler_enabled = false`. The existing staging/east/west roots default to `true` after bootstrap; override that explicitly until a new target has credentials and a successful manual execution. This creates the jobs, IAM bindings, and empty login secret containers without starting scheduled executions. For pre-merge deployment, dispatch the Deploy workflow from a branch in `superserve-ai/canaries` with `scope=ui-only`; this uses the existing GitHub cloud identities and applies only the UI modules. Fork branches cannot obtain those cloud credentials.
2. Populate `ui-canary-email-<target>` and `ui-canary-password-<target>` in the target project. Staging also requires `ui-canary-vercel-bypass-staging-us-central1`. Keep secret values out of Terraform and shell history; upload from a protected local file or through Secret Manager. The existing `api-canary-key-<target>` must belong to the **same team** as the UI login. Do not overwrite an existing API key merely to configure the UI canary.
3. Add the UI login as an active member of the existing API-canary team, with billing enabled and permission to create sandboxes. The deployed jobs select an exact `CANARY_UI_TEAM_LABEL` through the Console team switcher: `superserve.canary@gmail.com` in staging, `superserve.canary@gmail.com · US East` in production east, and `Canary West · US West` in production west. Missing or duplicate labels fail before creation. Update the configuration if a team is renamed. Without an explicit label, regional prefix configuration selects a unique regional team, or a single-team login uses its default team. Production jobs reject a prefixed sandbox ID from the wrong region. Legacy UUIDs must pass ownership tagging through the configured regional API; failures trigger UI cleanup.
4. Run each job and inspect its result:

```bash
gcloud run jobs execute ui-canary-staging-us-central1 --project rayai-dev --region us-central1 --wait
gcloud run jobs execute ui-canary-production-us-west2 --project rayai-prod --region us-west2 --wait
gcloud run jobs execute ui-canary-production-us-east4 --project rayai-prod --region us-east4 --wait
```

5. After successful live runs, commit `ui_scheduler_enabled = true` in each root's `variables.tf`. Normal deployments then preserve the five-minute schedule. Verify consecutive scheduled executions finish successfully and delete their sandboxes.

The jobs emit step metrics and execution logs. Failure screenshots currently remain on the container's temporary filesystem; durable artifact storage and alerting are separate follow-ups.

#### Browser regression tests

CI installs `playwright-core@1.57.0` from npm, matching `playwright-go v0.5700.1` and the Docker image. This avoids the Go installer's retired driver-download mirrors. `CANARY_REQUIRE_BROWSER_TESTS=true` turns a missing driver or Chromium into a test failure instead of silently skipping browser coverage.

## Target Inventory

Active canary targets:

| Target | Project | Region | API base URL | Preview domain |
| --- | --- | --- | --- | --- |
| staging | `rayai-dev` | `us-central1` | `https://api-staging.superserve.ai` | `staging-sandbox.superserve.ai` |
| production east | `rayai-prod` | `us-east4` | `https://api.superserve.ai` | `sandbox.superserve.ai` |
| production west | `rayai-prod` | `us-west2` | `https://api-usw.superserve.ai` | `usw-sandbox.superserve.ai` |

## Required Secrets

Terraform creates secret containers only. Populate versions manually after apply.

Expected API canary secret names:
- `api-canary-key-staging-us-central1`
- `api-canary-key-production-us-east4`
- `api-canary-key-production-us-west2`

Expected UI canary secret names:
- `ui-canary-email-staging-us-central1`
- `ui-canary-password-staging-us-central1`
- `ui-canary-vercel-bypass-staging-us-central1` (staging only)

Each API canary secret value must be a customer API key for a dedicated canary account or team.

Rotate credentials by:
1. create a new API key / password in Superserve
2. add a new Secret Manager version
3. rerun the Cloud Run Job or wait for the next schedule
4. revoke the old API key / credential

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
go run ./cmd/api-canary -mode template
go run ./cmd/api-canary -mode snapshot
go run ./cmd/api-canary -mode soak
go run ./cmd/api-canary -mode janitor
```

Manual staging execution creates and deletes real resources. Use a dedicated canary API key.

### Load runner

The load runner uses the same API credentials, lifecycle operations, metrics, and janitor ownership as the canaries. Daily jobs run 1,000 complete lifecycles with up to 100 workers per target. Each lifecycle creates a sandbox, waits for active, verifies a short command, pauses and waits for paused, resumes and verifies execution with fresh credentials, then deletes and waits for deletion. Production runs require `LOAD_TEST_PRODUCTION_OPT_IN=true`.

| Target | Daily start, America/Los_Angeles | Allowed window | Job |
| --- | --- | --- | --- |
| staging/us-central1 | 07:45 | 07:45–08:12 | `sandbox-daily-load-staging-us-central1` |
| production/us-east4 | 07:45 | 07:45–08:12 | `sandbox-daily-load-production-us-east4` |
| production/us-west2 | 08:15 | 08:15–08:42 | `sandbox-daily-load-production-us-west2` |

Staging and production use separate projects. A project-wide GCS lease serializes production regions. Scheduled runs have a 20-minute workload budget, two-minute cleanup budget, one minute reserved for reporting, a 25-minute Cloud Run task limit, and 45-minute leases. Late dispatches are rejected unless the full budget fits. A separate admission lease remains through the daily window after completion, preventing duplicate deliveries from starting another run. The work context has an absolute deadline before cleanup/reporting time, so delays cannot extend the assigned window. The runtime also borrows the existing target burst/dax/soak leases; it blocks if a benchmark is already active and prevents a new local heavy benchmark from racing the load. Routine lifecycle canaries continue independently.

Scheduling excludes the [public Friday TTI](https://github.com/computesdk/benchmarks/blob/master/.github/workflows/sandbox-tti-benchmarks.yml) and [DAX](https://github.com/computesdk/benchmarks/blob/master/.github/workflows/sandbox-dax-benchmarks.yml) windows (13:00/13:30 UTC with one-hour job limits and a buffer), observed private bursts near 00:00/01:00 and 17:00 UTC, and conservative historical Lindy windows around 10:55 UTC/07:00 Eastern and 09:00 Pacific. Lindy changed its workload in August, so these are historical exclusions rather than a verified current customer cron. A bounded public GitHub preflight also rejects active or queued ComputeSDK sandbox workflows; unavailable or incomplete status fails closed. [Continuous minute reliability probes](https://www.computesdk.com/benchmarks/sandboxes/reliability) are allowed. Private/on-demand work that is not visible in public metadata and ordinary shared-customer traffic cannot be fully isolated by this scheduler.

One final **Load Testing Results** summary goes to `#benchmark-results`, using the existing populated production webhook. The fenced monospace table lists create/pause/resume/delete/lifecycle p50, p95, p99 and successful sample counts in milliseconds. It also reports target, configured/peak concurrency, configured/started/succeeded/failed/unstarted sandboxes, duration, success rate, successful lifecycle throughput, failure stages, and cleanup failures. Exact untrimmed percentiles and throughput are computed from in-process observations, with no extra sandbox requests for aggregation. Failed and unattempted phases do not contribute invented zero latencies.

Failures, blocked runs, summary-delivery failures, metrics-export failures, startup/task failures, and Scheduler dispatch errors alert the existing `#on-call` channels. Per-operation errors do not trigger separate log notifications. There is no pre-first-run missing-result alert.

Cleanup runs independently after cancellation. Scheduled sandboxes have a one-minute active-session timeout and idle timeout to limit orphaned compute after a crashed runner; ownership and paused auto-delete retention remain two hours for the janitor. These platform reapers are best effort during failures. Uncertain creation outcomes or cleanup failures stop replacement dispatch rather than accumulating unobserved sandboxes above the worker limit. Verify canary-team quota headroom for 100 load workers plus routine canaries before enabling a new target; this workflow does not change team quotas.

Deployment first applies the narrow production secret-access grant for the existing staging load identity, then deploys staging, then production. UI-only deployment skips that grant and leaves load jobs/images alone. All roots require `load_runner_image`, pinned independently from API canary rollbacks; normal deployment supplies the shared image containing `/load-runner`. The default daily schedules are enabled after normal deployment. Set `daily_load_scheduler_enabled=false` to pause them.

For local/manual use, `LOAD_TEST_SCENARIO=create` remains the default and preserves the existing create/verify/delete workload; choose `lifecycle` for pause/resume and deletion-completion samples. The staging manual Cloud Run job retains its 100/10 defaults and uses shared/external guards without requiring the daily slot. Unguarded local runs remain explicit operator responsibility.

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
export PREVIEW_DOMAIN=sandbox.superserve.ai
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

### Mass launch scenario

`LOAD_TEST_SCENARIO=launch` measures how fast a fleet of sandboxes can be brought up. It is a strategy over the same create/verify workload, not a separate tool. Each operation creates a sandbox, waits for active, and runs one command; the sandbox stays resident until the launch (plus `LOAD_TEST_LAUNCH_HOLD`) ends. Then a bounded teardown deletes every resident sandbox and sweeps by run metadata, so creates with an unknown outcome are also removed. Cleanup is reported as verified only when the final list for the run is empty. When a create outcome is unknown, the sweep keeps listing for the longer of `HTTP_TIMEOUT` and `LOAD_TEST_LAUNCH_CREATE_SETTLE` (default 2m, which covers the server's own create bound) so a create that lands late is still removed. The sandbox TTL is the backstop for a create that lands later than that.

| Variable | Default | Meaning |
| --- | --- | --- |
| `LOAD_TEST_OPERATIONS` | 100 | Global sandbox count across all tasks. |
| `LOAD_TEST_LAUNCH_CEILING` | 100 | Safety ceiling. Runs above it fail at startup. Production requires an explicit value. |
| `LOAD_TEST_LAUNCH_STRATEGY` | `burst` | `burst` dispatches at `LOAD_TEST_LAUNCH_RATE` (0 = as fast as concurrency allows); `ramp` starts at `LOAD_TEST_RAMP_START_RATE` and adds `LOAD_TEST_RAMP_STEP_RATE` every `LOAD_TEST_RAMP_STEP_DURATION`, up to `LOAD_TEST_LAUNCH_RATE`. A non-zero rate must be at least 0.01/s per task. |
| `LOAD_TEST_CONCURRENCY` | min(256, task share) | In-flight creates per task. |
| `LOAD_TEST_API_RPS` | 40 | Aggregate API request budget (creates, polls, deletes), split across tasks. The default stays under the per-team API limit. It must be at least 0.01/s per task. |
| `LOAD_TEST_TEMPLATE` | `superserve/node-22` | Template; its vCPU, memory, disk, and image size are recorded in the report. |
| `LOAD_TEST_TEARDOWN_TIMEOUT` / `LOAD_TEST_TEARDOWN_CONCURRENCY` | 15m / 32 | Teardown budget. It is added to the cleanup budget, and the launch lease TTL defaults to one hour. |
| `LOAD_TEST_TASK_INDEX` / `LOAD_TEST_TASK_COUNT` | `CLOUD_RUN_TASK_INDEX` / `CLOUD_RUN_TASK_COUNT` | Fan-out. Task *i* runs global operations *i*, *i+count*, ... and reports worker `task-i`. One task runs at most 10,000 sandboxes. |
| `LOAD_TEST_LAUNCH_STAGES` | unset | Staged ramp: comma-separated cumulative counts, for example `1000,10000,50000,100000`. The last count must equal `LOAD_TEST_OPERATIONS`. |
| `LOAD_TEST_LAUNCH_STAGE_HOLD` | 2m | Hold at each stage boundary before the stage gate reads the guard. |

The report (`*Mass Launch Results*`) gives creates/s (average, peak 1 s, peak 10 s), ready/s, p50/p95/p99 for the create request, create to active, and create to first successful command, failures by class (`rate_limited`, `team_quota`, `host_cap`, `server_error`, `rejected`, `transport`, `canceled`, `active_failed`, `active_timeout`, `first_command`), failures at the 100, 1K, 10K, 100K, and 1M milestones, a 10 s timeline, and generator health (schedule lag, slot wait, API budget wait). The `Bottleneck` line separates generator limits (API budget, local concurrency) from API rate limits, team quota, host caps, guard backoffs, and platform errors. The `Correlate` line gives the exact UTC window and GMP queries for per-host restore rate, control-plane transitions, and DB pool load.

#### Staged ramp

With `LOAD_TEST_LAUNCH_STAGES` set, a launch runs as stages inside one run. At each stage boundary the task stops dispatching, waits until its in-flight creates finish, holds for `LOAD_TEST_LAUNCH_STAGE_HOLD`, and evaluates the guard once. If an enforcing guard backs off or aborts at that gate, the launch stops there, skips `LOAD_TEST_LAUNCH_HOLD`, and tears down. The sandboxes from earlier stages stay resident until teardown, so each stage adds load on top of the previous one. The report lists every gate with its counts, elapsed time, and guard result. In a fan-out run each task gates on its own share; tasks do not wait for each other.

#### Host pressure guard

The VMD scheduler does not gate on host vCPU or memory, so the launch scenario watches host pressure while it runs. Every `LOAD_TEST_GUARD_INTERVAL` (10s) it reads the configured sources and compares each signal with its thresholds:

- **Generator** (always on): create error ratio and create p99 over `LOAD_TEST_GUARD_CREATE_WINDOW`, after `LOAD_TEST_GUARD_CREATE_MIN_SAMPLES`.
- **Supabase `host_pressure`** (`LOAD_TEST_PRESSURE_SUPABASE_URL` + `LOAD_TEST_PRESSURE_SUPABASE_KEY`, read only, at most 64 rows per poll, and a larger fleet fails closed until `LOAD_TEST_GUARD_HOST_IDS` narrows it): cap headroom against `VMD_MAX_SANDBOXES` / `VMD_MAX_NETWORK_SLOTS` and the report age. The per-host cap table is printed in the report. Set `LOAD_TEST_GUARD_HOST_IDS` to the hosts under test so stale rows are ignored.
- **GMP** (`LOAD_TEST_GUARD_GMP_PROJECT`, ADC with the monitoring read scope): restore p99, restore error ratio, and DB pool use. Host free memory, PSI, CPU steal, and run queue are supported rules with no default query because no exporter publishes them yet; set `LOAD_TEST_GUARD_<SIGNAL>_QUERY` when one exists. Signals without a query are listed as unconfigured in the report. Each GMP read also runs `LOAD_TEST_GUARD_GMP_PRESENCE_QUERY` (default: hosts with a recent `host_conntrack_max` series, which vmd exports on every host), and fails closed if no host reports or if a host in `LOAD_TEST_GUARD_HOST_IDS` is missing. A value of `+Inf` (for example a restore p99 past the top histogram bucket) counts as a sample; `NaN` from an idle ratio is dropped.

A backoff halves the dispatch rate and concurrency (down to 1/16) and pauses for `LOAD_TEST_GUARD_BACKOFF_PAUSE`. A clear evaluation doubles the rate back. The guard evaluates once before the first create, and each create takes its API slot and re-checks the guard when it is dispatched, so a backoff also slows creates that waited for a slot. An unreachable source counts as a backoff, so the guard fails closed. So does a `host_pressure` read with no rows or with no row for a host in `LOAD_TEST_GUARD_HOST_IDS`, and a GMP read where no guarded query returns data. A single guarded query without data (for example a restore latency query while hosts are idle) is listed in the report but does not back off. An abort threshold or more than `LOAD_TEST_GUARD_MAX_BACKOFFS` consecutive backoffs stops dispatch, and teardown still runs. `LOAD_TEST_GUARD_MODE=observe` records triggers without acting. Override any threshold with `LOAD_TEST_GUARD_<SIGNAL>_BACKOFF` / `_ABORT` (for example `LOAD_TEST_GUARD_FREE_MEMORY_PCT_ABORT=5`). Every trigger is listed in the report with its time, host, value, and threshold.

#### Production requirements

A production launch needs all of the following, and the runner rejects the configuration otherwise: `LOAD_TEST_PRODUCTION_OPT_IN=true`, GCS coordination with the global lock and benchmark guard, an explicit `LOAD_TEST_LAUNCH_CEILING`, `LOAD_TEST_GUARD_MODE=enforce` with a Supabase or GMP source, and a single task (fan-out has no shared lease yet). The first milestone is 100K sandboxes, run as a staged ramp (for example `1000,10000,50000,100000`). Before it, run a small staging launch and a calibration launch of about 100 sandboxes to measure per-sandbox host memory and the create rate. Also confirm the team quota and API rate limits for the run, the per-host caps, the host count, the run window, and cleanup ownership. A 100K run is not possible with the current limits: it needs at least 10 tasks (10,000 per task), and production allows one task until shared leases exist.

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

The production roots deploy the daily lifecycle load jobs in east and west, using `/load-runner` and explicit production opt-in. Their benchmark summaries and on-call alerts use separate destinations as described above.

The east production region uses the public Google Telemetry API instead of the private collector path, so it does not need VPC access.

Checklist:
1. Confirm the production lifecycle and janitor jobs were created for both regions.
2. Populate the production Secret Manager secret versions with the canary API keys.
3. Verify the scheduled lifecycle executions start after the secrets are populated.
4. Inspect the job logs for the created run ID, sandbox ID, and cleanup outcome.
5. If a run fails or leaves a resource behind, execute the matching janitor job.
6. Retained sandboxes carry `retained_for_debug=true`, `failed_step`, `retained_at`, and `expires_at` metadata.

Exact production job commands:

```bash
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

## Preview Coverage

The lifecycle canary verifies deterministic public preview URLs. The soak scenario separately exercises authenticated preview URL creation, connectivity, and deletion through the customer API.
