# Canary Metrics Inventory

This repo exports OpenTelemetry metrics that are named directly in code and then
exposed to Managed Prometheus with the usual OpenTelemetry Prometheus suffixes.

Counters export as `*_total`.
Histograms export as `*_bucket`, `*_sum`, and `*_count`.
Gauges export as the instrument name as written.

## Inventory

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `superserve_canary_run_total` | Counter | `environment`, `region`, `target`, `scenario`, `result` | Counts completed canary runs for lifecycle and janitor modes. |
| `superserve_canary_step_total` | Counter | `environment`, `region`, `target`, `scenario`, `step`, `result` | Counts lifecycle step outcomes, including request and readiness phases for create, pause, resume, preview, and delete. |
| `superserve_canary_cleanup_total` | Counter | `environment`, `region`, `target`, `scenario`, `result` | Counts sandbox cleanup attempts from lifecycle finalization. |
| `superserve_canary_overlap_skipped_total` | Counter | `environment`, `region`, `target`, `scenario`, `result` | Counts lifecycle runs skipped because another run already held the target lock. |
| `superserve_canary_running_executions` | Up-down counter | `environment`, `region`, `target`, `scenario`, `result` | Tracks current in-flight executions per target and scenario. |
| `superserve_canary_orphan_resources` | Gauge | `environment`, `region`, `target`, `scenario`, `result` | Tracks the current unresolved orphan count observed by the janitor. |
| `superserve_canary_oldest_orphan_age_seconds` | Gauge | `environment`, `region`, `target`, `scenario`, `result` | Tracks the age of the oldest unresolved orphan, in seconds. |
| `superserve_canary_retained_sandbox_total` | Counter | `environment`, `region`, `target`, `scenario`, `step`, `result` | Counts lifecycle failures retained for debugging, grouped by failing step. |
| `superserve_canary_janitor_resources_examined_total` | Counter | `environment`, `region`, `target`, `scenario`, `result` | Counts retained sandboxes the janitor inspected. |
| `superserve_canary_janitor_resources_deleted_total` | Counter | `environment`, `region`, `target`, `scenario`, `result` | Counts retained sandboxes the janitor successfully deleted. |
| `superserve_canary_janitor_delete_failures_total` | Counter | `environment`, `region`, `target`, `scenario`, `result` | Counts janitor delete attempts that failed. |
| `superserve_canary_run_duration_seconds` | Histogram | `environment`, `region`, `target`, `scenario`, `result` | End-to-end run duration for lifecycle and janitor runs. |
| `superserve_canary_step_duration_seconds` | Histogram | `environment`, `region`, `target`, `scenario`, `step`, `result` | Lifecycle step duration histograms for create total, create request, create readiness, pause total, pause request, pause readiness, resume total, resume request, resume readiness, preview polling, and delete request. |
| `superserve_canary_last_completed_timestamp_seconds` | Gauge | `environment`, `region`, `target`, `scenario`, `result` | Timestamp of the last completed run, successful or failed. |
| `superserve_canary_last_success_timestamp_seconds` | Gauge | `environment`, `region`, `target`, `scenario`, `result` | Timestamp of the last successful run. |

## Notes

- No metric uses `sandbox_id`, `run_id`, `preview_url`, or `error` as a label.
- `failed_step` is no longer emitted as a separate label; retained failures use the bounded `step` label instead.
- `create_total` and `pause_total` capture the full user wait for the operation, while `resume_total` runs through the first successful post-resume command to better reflect when the sandbox is usable. The corresponding `*_request` and `*_wait_*` steps remain available for diagnostics.
- `cleanup_total` is sufficient for cleanup success/failure panels by filtering `result`.
- `run_total` is sufficient for janitor success/failure panels by filtering `scenario="janitor"` and `result`.
