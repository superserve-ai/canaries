locals {
  use_direct_vpc = var.vpc_connector == null && var.vpc_network != null && var.vpc_subnetwork != null
  use_vpc_access = var.vpc_connector != null || local.use_direct_vpc

  name_suffix         = var.scenario == "lifecycle" ? "" : "-${var.scenario}"
  lifecycle_job_name  = "api-canary${local.name_suffix}-${var.target_name}"
  scheduler_name      = "api-canary${local.name_suffix}-schedule-${var.target_name}"
  runtime_sa_prefix   = { lifecycle = "apicn", template = "apicnt", snapshot = "apicnsnap" }[var.scenario]
  scheduler_sa_prefix = { lifecycle = "apicns", template = "apicnts", snapshot = "apicnsnaps" }[var.scenario]
  api_key_secret_id   = var.create_api_key_secret ? google_secret_manager_secret.api_key[0].secret_id : var.api_key_secret_name
  lifecycle_run_logs_query = format(
    "resource.type%%3D%%22cloud_run_job%%22%%0Aresource.labels.job_name%%3D%%22%s%%22%%0Alabels.%%22run.googleapis.com/execution_name%%22%%3D%%22$${log.extracted_label.execution_name}%%22",
    google_cloud_run_v2_job.lifecycle.name,
  )
  lifecycle_job_logs_query = format(
    "resource.type%%3D%%22cloud_run_job%%22%%0Aresource.labels.job_name%%3D%%22%s%%22",
    google_cloud_run_v2_job.lifecycle.name,
  )
  labels = merge(var.labels, {
    environment = var.environment
    region      = var.target_region
    managed_by  = "terraform"
    component   = "api-canary${local.name_suffix}"
    target      = var.target_name
  })
}

resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = substr("${local.runtime_sa_prefix}-${var.target_name}", 0, 30)
  display_name = "API Canary${local.name_suffix} ${var.target_name}"
}

resource "google_service_account" "scheduler" {
  project      = var.project_id
  account_id   = substr("${local.scheduler_sa_prefix}-${var.target_name}", 0, 30)
  display_name = "API Canary${local.name_suffix} Scheduler ${var.target_name}"
}

# The lifecycle instance owns the per-target secret; other scenarios on the
# same target only reference it.
moved {
  from = google_secret_manager_secret.api_key
  to   = google_secret_manager_secret.api_key[0]
}

resource "google_secret_manager_secret" "api_key" {
  count     = var.create_api_key_secret ? 1 : 0
  project   = var.project_id
  secret_id = var.api_key_secret_name

  replication {
    auto {}
  }

  labels = local.labels
}

resource "google_secret_manager_secret_iam_member" "runtime_accessor" {
  project   = var.project_id
  secret_id = local.api_key_secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_cloud_run_v2_job" "lifecycle" {
  project             = var.project_id
  name                = local.lifecycle_job_name
  location            = var.job_region
  labels              = local.labels
  deletion_protection = false
  depends_on = [
    google_secret_manager_secret_iam_member.runtime_accessor
  ]

  template {
    labels = local.labels

    template {
      service_account = google_service_account.runtime.email
      timeout         = var.job_timeout
      max_retries     = 0
      dynamic "vpc_access" {
        for_each = local.use_vpc_access ? [1] : []

        content {
          connector = var.vpc_connector
          egress    = var.vpc_egress

          dynamic "network_interfaces" {
            for_each = local.use_direct_vpc ? [1] : []

            content {
              network    = var.vpc_network
              subnetwork = var.vpc_subnetwork
              tags       = var.vpc_tags
            }
          }
        }
      }
      containers {
        image = var.image
        args  = ["-mode", var.scenario]

        env {
          name  = "CANARY_MODE"
          value = var.scenario
        }
        env {
          name  = "RUN_TIMEOUT"
          value = var.run_timeout
        }
        env {
          name  = "LOCK_TTL"
          value = var.lock_ttl
        }
        env {
          name  = "CANARY_RUNTIME"
          value = "cloud-run"
        }
        env {
          name  = "CANARY_METRICS_EXPORTER"
          value = "otlp"
        }
        env {
          name  = "CANARY_LOCK_BACKEND"
          value = "gcs"
        }
        env {
          name  = "CANARY_RETAIN_FAILED_SANDBOX"
          value = tostring(var.retain_failed_sandbox)
        }
        env {
          name  = "CANARY_RETAIN_FAILED_SANDBOX_TTL"
          value = var.retain_failed_sandbox_ttl
        }
        env {
          name  = "CANARY_TARGET"
          value = var.target_name
        }
        env {
          name  = "CANARY_SANDBOX_TEMPLATE"
          value = "superserve/python-3.11"
        }
        env {
          name  = "CANARY_ENVIRONMENT"
          value = var.environment
        }
        env {
          name  = "CANARY_REGION"
          value = var.target_region
        }
        env {
          name  = "GCP_PROJECT_ID"
          value = var.project_id
        }
        env {
          name  = "API_BASE_URL"
          value = var.api_base_url
        }
        env {
          name  = "PREVIEW_DOMAIN"
          value = var.preview_domain
        }
        env {
          name  = "LOCK_BUCKET"
          value = var.lock_bucket_name
        }
        env {
          name  = "OTEL_SERVICE_NAME"
          value = "superserve-api-canary"
        }
        env {
          name  = "OTEL_ENVIRONMENT"
          value = var.environment
        }
        env {
          name = "OTEL_RESOURCE_ATTRIBUTES"
          value = join(",", [
            "gcp.project_id=${var.project_id}",
            "cloud.region=${var.target_region}",
            "deployment.environment.name=${var.environment}",
          ])
        }
        env {
          name  = "OTEL_EXPORTER_OTLP_ENDPOINT"
          value = var.otlp_metrics_endpoint
        }
        env {
          name  = "MANUAL_STAGING_OPT_IN"
          value = tostring(var.manual_staging_opt_in)
        }
        env {
          name = "CANARY_API_KEY"
          value_source {
            secret_key_ref {
              secret  = local.api_key_secret_id
              version = "latest"
            }
          }
        }
      }
    }
  }
}

resource "google_cloud_run_v2_job_iam_member" "scheduler_invoker" {
  count    = var.scheduler_enabled ? 1 : 0
  project  = var.project_id
  location = var.job_region
  name     = google_cloud_run_v2_job.lifecycle.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}

resource "google_cloud_scheduler_job" "lifecycle" {
  count       = var.scheduler_enabled ? 1 : 0
  project     = var.project_id
  region      = var.job_region
  name        = local.scheduler_name
  description = "Runs API ${var.scenario} canary for ${var.target_name}"
  schedule    = var.scheduler_cron
  time_zone   = "Etc/UTC"

  http_target {
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.job_region}/jobs/${google_cloud_run_v2_job.lifecycle.name}:run"
    http_method = "POST"

    oauth_token {
      service_account_email = google_service_account.scheduler.email
      scope                 = "https://www.googleapis.com/auth/cloud-platform"
    }
  }
}

resource "google_monitoring_alert_policy" "cloud_run_job_failed" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "API Canary ${var.target_name}: ${var.scenario} failed"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  conditions {
    display_name = "${title(var.scenario)} terminal failure log"

    condition_matched_log {
      filter = <<-EOT
        resource.type="cloud_run_job"
        AND resource.labels.job_name="${google_cloud_run_v2_job.lifecycle.name}"
        AND jsonPayload.message="${var.scenario} canary completed"
        AND jsonPayload.result="failure"
      EOT

      label_extractors = {
        execution_name = "EXTRACT(labels.\"run.googleapis.com/execution_name\")"
        failed_step    = "EXTRACT(jsonPayload.failed_step)"
        sandbox_id     = "EXTRACT(jsonPayload.sandbox_id)"
        template_id    = "EXTRACT(jsonPayload.template_id)"
        build_id       = "EXTRACT(jsonPayload.build_id)"
        build_error    = "EXTRACT(jsonPayload.build_error)"
      }
    }
  }

  alert_strategy {
    notification_rate_limit {
      period = "300s"
    }

    auto_close = "1800s"
  }

  documentation {
    content   = <<-EOT
      API Canary ${var.target_name} ${var.scenario} failed

      Region: ${var.target_region}
      Sandbox: $${log.extracted_label.sandbox_id}
      Failed step: $${log.extracted_label.failed_step}
%{if var.scenario == "template"~}
      Template: $${log.extracted_label.template_id}
      Build: $${log.extracted_label.build_id}
      Build error: $${log.extracted_label.build_error}
%{endif}

      [View canary run logs](https://console.cloud.google.com/logs/query;query=${local.lifecycle_run_logs_query};project=${var.project_id})
      EOT
    mime_type = "text/markdown"
  }

  user_labels = local.labels
}

resource "google_monitoring_alert_policy" "overlap_skipped" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "API Canary ${var.target_name}: overlapping run skipped"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  conditions {
    display_name = "${title(var.scenario)} run skipped because target lock was already held"

    condition_prometheus_query_language {
      query                     = "((sum(max_over_time(superserve_canary_overlap_skipped_total{target=\"${var.target_name}\",scenario=\"${var.scenario}\"}[15m])) or vector(0)) > 0)"
      duration                  = "0s"
      disable_metric_validation = true
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    content   = <<-EOT
      API Canary ${var.target_name} skipped a ${var.scenario} run because another execution already held the target lock.

      This indicates overlapping executions rather than a ${var.scenario} failure.

      [View Cloud Run job logs](https://console.cloud.google.com/logs/query;query=${local.lifecycle_job_logs_query};project=${var.project_id})
      EOT
    mime_type = "text/markdown"
  }

  user_labels = local.labels
}

resource "google_monitoring_alert_policy" "missing_runs" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "API Canary ${var.target_name}: missing completed runs"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  conditions {
    display_name = "No ${var.scenario} success or failure metric in ${var.missing_runs_window}"

    condition_prometheus_query_language {
      query                     = "((sum(increase(superserve_canary_run_total{target=\"${var.target_name}\",scenario=\"${var.scenario}\",result=~\"success|failure\"}[${var.missing_runs_window}])) or vector(0)) == 0)"
      duration                  = "0s"
      disable_metric_validation = true
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    content   = <<-EOT
      The scheduler, Cloud Run Job, or metrics export path for ${var.target_name} may be stalled.

      [View Cloud Run job logs](https://console.cloud.google.com/logs/query;query=${local.lifecycle_job_logs_query};project=${var.project_id})
      EOT
    mime_type = "text/markdown"
  }

  user_labels = local.labels
}
