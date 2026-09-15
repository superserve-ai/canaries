locals {
  use_direct_vpc = var.vpc_connector == null && var.vpc_network != null && var.vpc_subnetwork != null
  use_vpc_access = var.vpc_connector != null || local.use_direct_vpc

  job_name       = "ui-canary-${var.target_name}"
  scheduler_name = "ui-canary-schedule-${var.target_name}"

  labels = merge(var.labels, {
    environment = var.environment
    region      = var.target_region
    managed_by  = "terraform"
    component   = "ui-canary"
    target      = var.target_name
  })
}

resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = substr("uicn-${var.target_name}", 0, 30)
  display_name = "UI Canary ${var.target_name}"
}

resource "google_service_account" "scheduler" {
  project      = var.project_id
  account_id   = substr("uicns-${var.target_name}", 0, 30)
  display_name = "UI Canary Scheduler ${var.target_name}"
}

resource "google_secret_manager_secret" "ui_email" {
  project   = var.project_id
  secret_id = var.ui_email_secret_name

  replication {
    auto {}
  }

  labels = local.labels
}

resource "google_secret_manager_secret" "ui_password" {
  project   = var.project_id
  secret_id = var.ui_password_secret_name

  replication {
    auto {}
  }

  labels = local.labels
}

resource "google_secret_manager_secret" "ui_vercel_bypass" {
  count     = var.ui_vercel_bypass_secret_name != null ? 1 : 0
  project   = var.project_id
  secret_id = var.ui_vercel_bypass_secret_name

  replication {
    auto {}
  }

  labels = local.labels
}

resource "google_secret_manager_secret_iam_member" "email_accessor" {
  project   = var.project_id
  secret_id = google_secret_manager_secret.ui_email.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_secret_manager_secret_iam_member" "password_accessor" {
  project   = var.project_id
  secret_id = google_secret_manager_secret.ui_password.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_secret_manager_secret_iam_member" "vercel_bypass_accessor" {
  count     = var.ui_vercel_bypass_secret_name != null ? 1 : 0
  project   = var.project_id
  secret_id = google_secret_manager_secret.ui_vercel_bypass[0].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_secret_manager_secret_iam_member" "api_key_accessor" {
  project   = var.project_id
  secret_id = var.api_key_secret_name
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_storage_bucket_iam_member" "lock_admin" {
  bucket = var.lock_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_cloud_run_v2_job" "ui_canary" {
  project             = var.project_id
  name                = local.job_name
  location            = var.job_region
  labels              = local.labels
  deletion_protection = false
  depends_on = [
    google_secret_manager_secret_iam_member.email_accessor,
    google_secret_manager_secret_iam_member.password_accessor,
    google_secret_manager_secret_iam_member.api_key_accessor,
    google_secret_manager_secret_iam_member.vercel_bypass_accessor,
  ]

  template {
    labels = local.labels

    template {
      service_account = google_service_account.runtime.email
      timeout         = "600s"
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
        args  = ["-mode", "ui-lifecycle"]

        resources {
          limits = {
            cpu    = "1"
            memory = "2Gi"
          }
        }

        env {
          name  = "CANARY_MODE"
          value = "ui-lifecycle"
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
          name  = "LOCK_BUCKET"
          value = var.lock_bucket_name
        }
        env {
          name  = "CANARY_TARGET"
          value = var.target_name
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
          name  = "CANARY_UI_CONSOLE_URL"
          value = var.console_url
        }
        env {
          name  = "CANARY_UI_HEADLESS"
          value = "true"
        }
        env {
          name  = "API_BASE_URL"
          value = var.api_base_url
        }
        env {
          name  = "OTEL_SERVICE_NAME"
          value = "superserve-ui-canary"
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
          name = "CANARY_UI_EMAIL"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.ui_email.secret_id
              version = "latest"
            }
          }
        }

        env {
          name = "CANARY_UI_PASSWORD"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.ui_password.secret_id
              version = "latest"
            }
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

        dynamic "env" {
          for_each = var.ui_vercel_bypass_secret_name != null ? [1] : []
          content {
            name = "CANARY_UI_VERCEL_PROTECTION_BYPASS"
            value_source {
              secret_key_ref {
                secret  = google_secret_manager_secret.ui_vercel_bypass[0].secret_id
                version = "latest"
              }
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
  name     = google_cloud_run_v2_job.ui_canary.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}

resource "google_cloud_scheduler_job" "ui_canary" {
  count       = var.scheduler_enabled ? 1 : 0
  project     = var.project_id
  region      = var.job_region
  name        = local.scheduler_name
  description = "Runs UI lifecycle canary for ${var.target_name}"
  schedule    = var.scheduler_cron
  time_zone   = "Etc/UTC"

  http_target {
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.job_region}/jobs/${google_cloud_run_v2_job.ui_canary.name}:run"
    http_method = "POST"

    oauth_token {
      service_account_email = google_service_account.scheduler.email
      scope                 = "https://www.googleapis.com/auth/cloud-platform"
    }
  }
}
