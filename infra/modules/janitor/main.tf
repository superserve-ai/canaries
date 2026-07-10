locals {
  job_name = "api-canary-janitor-${var.target_name}"
  labels = merge(var.labels, {
    environment = var.environment
    managed_by  = "terraform"
    component   = "api-canary-janitor"
    target      = var.target_name
  })
}

resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = substr("apicnjan-${var.target_name}", 0, 30)
  display_name = "API Canary Janitor ${var.target_name}"
}

resource "google_secret_manager_secret_iam_member" "runtime_accessor" {
  project   = var.project_id
  secret_id = var.api_key_secret_name
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_cloud_run_v2_job" "janitor" {
  project  = var.project_id
  name     = local.job_name
  location = var.job_region
  labels   = local.labels

  template {
    labels = local.labels

    template {
      service_account = google_service_account.runtime.email
      timeout         = "600s"
      max_retries     = 0

      containers {
        image = var.image
        args  = ["-mode", "janitor"]

        env {
          name  = "CANARY_MODE"
          value = "janitor"
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
          value = var.job_region
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
          name  = "OTEL_SERVICE_NAME"
          value = "superserve-api-canary"
        }
        env {
          name  = "OTEL_ENVIRONMENT"
          value = var.environment
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
      }
    }
  }
}

resource "google_monitoring_alert_policy" "janitor_failure" {
  project               = var.project_id
  display_name          = "API Canary Janitor ${var.target_name}: failed or stale"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids

  conditions {
    display_name = "Janitor failures in 24h"
    condition_prometheus_query_language {
      query    = "increase(superserve_canary_run_total{target=\"${var.target_name}\",scenario=\"janitor\",result=\"failure\"}[24h]) > 0"
      duration = "0s"
    }
  }

  conditions {
    display_name = "Janitor observed stale resources"
    condition_prometheus_query_language {
      query    = "max_over_time(superserve_canary_orphan_resources{target=\"${var.target_name}\"}[24h]) > 0"
      duration = "0s"
    }
  }

  user_labels = local.labels
}
