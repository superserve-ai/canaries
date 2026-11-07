resource "google_monitoring_alert_policy" "terminal_failure" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "Daily Load ${var.target_name}: run or summary delivery failed"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  lifecycle {
    precondition {
      condition     = length(var.notification_channel_ids) > 0
      error_message = "Daily load alerts require an existing on-call notification channel."
    }
  }

  conditions {
    display_name = "Terminal failure, configuration error, or startup error"
    condition_matched_log {
      # Select terminal events only: operation and cleanup logs can be numerous.
      filter = <<-EOT
        resource.type="cloud_run_job"
        AND resource.labels.job_name="${google_cloud_run_v2_job.daily.name}"
        AND (jsonPayload.message="load runner failed"
          OR (jsonPayload.message="load test completed" AND jsonPayload.result="failure"))
      EOT
      label_extractors = {
        execution_name = "EXTRACT(labels.\"run.googleapis.com/execution_name\")"
        run_id         = "EXTRACT(jsonPayload.run_id)"
        error          = "EXTRACT(jsonPayload.error)"
      }
    }
  }

  alert_strategy {
    notification_rate_limit { period = "300s" }
    auto_close = "1800s"
  }
  documentation {
    content   = <<-EOT
      The daily load run for ${var.target_name} failed to start, complete operations, clean up, or deliver its benchmark summary.
      Run ID: $${log.extracted_label.run_id}
      Error: $${log.extracted_label.error}

      [View execution logs](https://console.cloud.google.com/logs/query;query=${local.run_logs_query};project=${var.project_id})
      Check the terminal summary, cleanup failures, API credentials, GCS locks, OTLP connectivity, and reporting webhook.
    EOT
    mime_type = "text/markdown"
  }
  user_labels = local.labels
}

# The platform metric also covers image/secret startup errors, OOM, and task
# timeouts that prevent the binary from emitting a terminal result.
resource "google_monitoring_alert_policy" "execution_failure" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "Daily Load ${var.target_name}: Cloud Run execution failed"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  conditions {
    display_name = "Cloud Run reports a failed execution"
    condition_threshold {
      filter = join(" AND ", [
        "resource.type=\"cloud_run_job\"",
        "resource.labels.job_name=\"${google_cloud_run_v2_job.daily.name}\"",
        "metric.type=\"run.googleapis.com/job/completed_execution_count\"",
        "metric.labels.result=\"failed\"",
      ])
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "0s"
      aggregations {
        alignment_period     = "300s"
        per_series_aligner   = "ALIGN_SUM"
        cross_series_reducer = "REDUCE_SUM"
      }
      trigger { count = 1 }
    }
  }

  alert_strategy { auto_close = "1800s" }
  documentation {
    content   = "Cloud Run reports a failed daily load execution for ${var.target_name}. Inspect ${local.job_name} in ${var.job_region} for image or secret startup failures, task timeouts, memory exhaustion, or terminal application errors."
    mime_type = "text/markdown"
  }
  user_labels = local.labels
}

resource "google_monitoring_alert_policy" "scheduler_failure" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "Daily Load ${var.target_name}: Scheduler dispatch failed"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  conditions {
    display_name = "Cloud Scheduler failed to dispatch the load job"
    condition_matched_log {
      filter = <<-EOT
        resource.type="cloud_scheduler_job"
        AND resource.labels.job_id="${google_cloud_scheduler_job.daily.name}"
        AND severity>=ERROR
      EOT
    }
  }

  alert_strategy {
    notification_rate_limit { period = "300s" }
    auto_close = "1800s"
  }
  documentation {
    content   = "Cloud Scheduler failed to invoke the daily load job for ${var.target_name}. Inspect ${local.scheduler_name} in ${var.job_region}, its OAuth identity, and the job-scoped run.invoker grant. A late manual retry may be rejected by the runtime window guard; do not widen the window to force a run."
    mime_type = "text/markdown"
  }
  user_labels = local.labels
}
