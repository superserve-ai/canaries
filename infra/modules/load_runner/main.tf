locals {
  job_name       = "sandbox-daily-load-${var.target_name}"
  scheduler_name = "sandbox-daily-load-schedule-${var.target_name}"
  runtime_email  = var.create_runtime_service_account ? google_service_account.runtime[0].email : var.runtime_service_account_email
  use_direct_vpc = var.vpc_connector == null && var.vpc_network != null && var.vpc_subnetwork != null
  use_vpc_access = var.vpc_connector != null || local.use_direct_vpc
  labels = merge(var.labels, {
    environment = var.environment
    region      = var.target_region
    target      = var.target_name
    component   = "sandbox-daily-load"
    managed_by  = "terraform"
  })
  run_logs_query = format(
    "resource.type%%3D%%22cloud_run_job%%22%%0Aresource.labels.job_name%%3D%%22%s%%22%%0Alabels.%%22run.googleapis.com/execution_name%%22%%3D%%22$${log.extracted_label.execution_name}%%22",
    local.job_name,
  )
  environment_variables = {
    CANARY_ENVIRONMENT                 = var.environment
    CANARY_REGION                      = var.target_region
    CANARY_TARGET                      = var.target_name
    CANARY_RUNTIME                     = "cloud-run"
    CANARY_METRICS_EXPORTER            = "otlp"
    CANARY_LOCK_BACKEND                = "gcs"
    GCP_PROJECT_ID                     = var.project_id
    API_BASE_URL                       = var.api_base_url
    PREVIEW_DOMAIN                     = var.preview_domain
    LOCK_BUCKET                        = var.lock_bucket_name
    LOAD_TEST_SCENARIO                 = "lifecycle"
    LOAD_TEST_OPERATIONS               = tostring(var.load_operations)
    LOAD_TEST_CONCURRENCY              = tostring(var.load_concurrency)
    LOAD_TEST_RUN_TIMEOUT              = "20m"
    LOAD_TEST_RESOURCE_TTL             = "2h"
    LOAD_TEST_LOCK_TTL                 = "45m"
    LOAD_TEST_SCHEDULED                = "true"
    LOAD_TEST_TIME_ZONE                = var.scheduler_time_zone
    LOAD_TEST_DAILY_WINDOW_START       = var.daily_window_start
    LOAD_TEST_DAILY_WINDOW_END         = var.daily_window_end
    LOAD_TEST_PRODUCTION_OPT_IN        = tostring(var.environment == "production")
    LOAD_TEST_SANDBOX_TEMPLATE         = "superserve/python-3.11"
    LOAD_TEST_BENCHMARK_GUARD          = "true"
    LOAD_TEST_GLOBAL_LOCK              = "true"
    LOAD_TEST_EXTERNAL_BENCHMARK_GUARD = "true"
    DELETE_TIMEOUT                     = "2m"
    OTEL_SERVICE_NAME                  = "superserve-sandbox-load-runner"
    OTEL_ENVIRONMENT                   = var.environment
    OTEL_EXPORTER_OTLP_ENDPOINT        = var.otlp_metrics_endpoint
    OTEL_RESOURCE_ATTRIBUTES = join(",", [
      "gcp.project_id=${var.project_id}",
      "cloud.region=${var.target_region}",
      "deployment.environment.name=${var.environment}",
    ])
  }
}

resource "google_service_account" "runtime" {
  count        = var.create_runtime_service_account ? 1 : 0
  project      = var.project_id
  account_id   = "load-${var.target_name}"
  display_name = "Daily Load Runner ${var.target_name}"
}

resource "google_service_account" "scheduler" {
  project      = var.project_id
  account_id   = "loads-${var.target_name}"
  display_name = "Daily Load Scheduler ${var.target_name}"
}

resource "google_secret_manager_secret_iam_member" "api_key_accessor" {
  count     = var.manage_api_key_access ? 1 : 0
  project   = var.project_id
  secret_id = var.api_key_secret_name
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${local.runtime_email}"
}

resource "google_secret_manager_secret_iam_member" "slack_webhook_accessor" {
  count     = var.manage_slack_webhook_access ? 1 : 0
  project   = var.project_id
  secret_id = var.slack_webhook_secret_name
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${local.runtime_email}"
}

resource "google_storage_bucket_iam_member" "lock_admin" {
  bucket = var.lock_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${local.runtime_email}"
}

resource "google_project_iam_member" "runtime" {
  for_each = toset(["roles/monitoring.metricWriter", "roles/serviceusage.serviceUsageConsumer"])
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${local.runtime_email}"
}

resource "google_cloud_run_v2_job" "daily" {
  project             = var.project_id
  name                = local.job_name
  location            = var.job_region
  deletion_protection = false
  labels              = local.labels
  depends_on = [
    google_secret_manager_secret_iam_member.api_key_accessor,
    google_secret_manager_secret_iam_member.slack_webhook_accessor,
    google_storage_bucket_iam_member.lock_admin,
    google_project_iam_member.runtime,
  ]

  lifecycle {
    precondition {
      condition     = var.create_runtime_service_account || var.runtime_service_account_email != null
      error_message = "An existing runtime service account is required when account creation is disabled."
    }
    precondition {
      condition     = var.load_concurrency <= var.load_operations
      error_message = "load_concurrency must not exceed load_operations."
    }
  }

  template {
    labels      = local.labels
    task_count  = 1
    parallelism = 1

    template {
      service_account = local.runtime_email
      timeout         = "1500s"
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
        image   = var.image
        command = ["/load-runner"]

        dynamic "env" {
          for_each = local.environment_variables
          content {
            name  = env.key
            value = env.value
          }
        }

        env {
          name = "CANARY_API_KEY"
          value_source {
            secret_key_ref {
              secret  = var.api_key_secret_name
              version = "latest"
            }
          }
        }
        env {
          name = "CANARY_SLACK_WEBHOOK_URL"
          value_source {
            secret_key_ref {
              secret  = var.slack_webhook_secret_name
              version = "latest"
            }
          }
        }
      }
    }
  }
}

resource "google_cloud_run_v2_job_iam_member" "scheduler_invoker" {
  project  = var.project_id
  location = var.job_region
  name     = google_cloud_run_v2_job.daily.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}

resource "google_cloud_scheduler_job" "daily" {
  project          = var.project_id
  region           = var.job_region
  name             = local.scheduler_name
  description      = "Runs the daily ${var.load_operations}-operation / ${var.load_concurrency}-concurrent lifecycle load for ${var.target_name}"
  schedule         = var.scheduler_cron
  time_zone        = var.scheduler_time_zone
  paused           = !var.scheduler_enabled
  attempt_deadline = "60s"
  depends_on       = [google_cloud_run_v2_job_iam_member.scheduler_invoker]

  # Late or duplicate dispatches are rejected by the runtime's window and GCS guards.
  retry_config {
    retry_count        = 0
    max_retry_duration = "0s"
  }

  http_target {
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.job_region}/jobs/${google_cloud_run_v2_job.daily.name}:run"
    http_method = "POST"
    oauth_token {
      service_account_email = google_service_account.scheduler.email
      scope                 = "https://www.googleapis.com/auth/cloud-platform"
    }
  }
}
