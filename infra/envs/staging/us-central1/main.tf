removed {
  from = google_logging_project_bucket_config.alert_sql

  lifecycle {
    destroy = false
  }
}

removed {
  from = google_logging_linked_dataset.alert_sql

  lifecycle {
    destroy = false
  }
}

locals {
  labels = {
    environment = "staging"
    region      = "us-central1"
    managed_by  = "terraform"
  }

  deployment_alerting_roles = toset([
    "roles/logging.configWriter",
    "roles/monitoring.alertPolicyEditor",
    "roles/monitoring.notificationChannelEditor",
  ])

  otlp_endpoint             = "http://10.0.0.2:4318"
  retain_failed_sandbox     = true
  retain_failed_sandbox_ttl = "2h"

  dashboards = {
    canary = {
      display_name = "staging canary dashboard"
      definition = templatefile(abspath("${path.module}/../../../dashboards/cloud-monitoring/canary-overview.json.tftpl"), {
        project_id               = var.project_id
        environment              = local.labels.environment
        default_region           = "us-central1"
        regions                  = ["us-central1"]
        janitor_job_name_pattern = "api-canary-janitor-staging-us-central1"
      })
    }
  }
}

resource "google_storage_bucket" "locks" {
  project                     = var.project_id
  name                        = "${var.project_id}-api-canary-locks"
  location                    = "US-CENTRAL1"
  uniform_bucket_level_access = true
  labels                      = local.labels
}

module "lifecycle" {
  source = "../../../modules/canary_target"

  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "staging-us-central1"
  environment               = "staging"
  target_region             = "us-central1"
  api_base_url              = "https://api-staging.superserve.ai"
  preview_domain            = "staging-sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = "api-canary-key-staging-us-central1"
  lock_bucket_name          = google_storage_bucket.locks.name
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  manual_staging_opt_in     = true
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  create_alerts             = var.create_alerts
  vpc_connector             = "projects/rayai-dev/locations/us-central1/connectors/ss-vpc-conn-f1b3552"
  vpc_egress                = "ALL_TRAFFIC"
  depends_on                = [google_project_iam_member.deployment_alerting]
}

resource "google_service_account" "load_runner" {
  project      = var.project_id
  account_id   = "sbx-load-runner-staging"
  display_name = "Sandbox Load Runner staging"
}

resource "google_secret_manager_secret_iam_member" "load_runner_staging_accessor" {
  project   = var.project_id
  secret_id = module.lifecycle.api_key_secret_name
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.load_runner.email}"
}

resource "google_cloud_run_v2_job" "load_runner" {
  project             = var.project_id
  name                = "sandbox-load-runner-staging-us-central1"
  location            = var.job_region
  deletion_protection = false
  labels = merge(local.labels, {
    component = "sandbox-load-runner"
  })
  depends_on = [
    module.lifecycle,
    google_secret_manager_secret_iam_member.load_runner_staging_accessor,
  ]

  template {
    labels = merge(local.labels, {
      component = "sandbox-load-runner"
    })

    template {
      service_account = google_service_account.load_runner.email
      timeout         = "3600s"
      max_retries     = 0

      vpc_access {
        connector = "projects/rayai-dev/locations/us-central1/connectors/ss-vpc-conn-f1b3552"
        egress    = "ALL_TRAFFIC"
      }

      containers {
        image   = var.load_runner_image
        command = ["/load-runner"]

        # This deployment is staging-only by configuration, but the binary is
        # environment-agnostic and can be deployed separately for production.
        env {
          name  = "CANARY_ENVIRONMENT"
          value = "staging"
        }
        env {
          name  = "CANARY_REGION"
          value = "us-central1"
        }
        env {
          name  = "CANARY_TARGET"
          value = "staging-us-central1"
        }
        env {
          name  = "API_BASE_URL"
          value = "https://api-staging.superserve.ai"
        }
        env {
          name  = "PREVIEW_DOMAIN"
          value = "staging-sandbox.superserve.ai"
        }
        env {
          name  = "LOAD_TEST_OPERATIONS"
          value = "100"
        }
        env {
          name  = "LOAD_TEST_CONCURRENCY"
          value = "10"
        }
        env {
          name  = "LOAD_TEST_RESOURCE_TTL"
          value = "2h"
        }
        env {
          name  = "LOAD_TEST_RUN_TIMEOUT"
          value = "30m"
        }
        env {
          name  = "LOAD_TEST_SANDBOX_TEMPLATE"
          value = "superserve/python-3.11"
        }
        env {
          name  = "OTEL_SERVICE_NAME"
          value = "superserve-sandbox-load-runner"
        }
        env {
          name  = "OTEL_ENVIRONMENT"
          value = "staging"
        }
        env {
          name  = "OTEL_EXPORTER_OTLP_ENDPOINT"
          value = local.otlp_endpoint
        }
        env {
          name = "CANARY_API_KEY_STAGING"
          value_source {
            secret_key_ref {
              secret  = module.lifecycle.api_key_secret_name
              version = "latest"
            }
          }
        }
      }
    }
  }
}

module "janitor" {
  source = "../../../modules/janitor"

  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "staging-us-central1"
  environment               = "staging"
  api_base_url              = "https://api-staging.superserve.ai"
  preview_domain            = "staging-sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = "api-canary-key-staging-us-central1"
  lock_bucket_name          = google_storage_bucket.locks.name
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  create_alerts             = var.create_alerts
  vpc_connector             = "projects/rayai-dev/locations/us-central1/connectors/ss-vpc-conn-f1b3552"
  vpc_egress                = "ALL_TRAFFIC"
  depends_on                = [google_project_iam_member.deployment_alerting]
}

module "dashboard" {
  source = "../../../modules/dashboard"

  project_id = var.project_id
  dashboards = local.dashboards
}

module "permissions" {
  source = "../../../modules/permissions"

  project_id                              = var.project_id
  lock_bucket_name                        = google_storage_bucket.locks.name
  lifecycle_runtime_service_account_email = module.lifecycle.runtime_service_account_email
  janitor_runtime_service_account_email   = module.janitor.runtime_service_account_email
}

resource "google_project_iam_member" "deployment_alerting" {
  for_each = local.deployment_alerting_roles

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${var.deployment_service_account_email}"
}

resource "google_monitoring_alert_policy" "metrics_shutdown_failed" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "staging API canary: metrics shutdown failed"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids
  depends_on            = [google_project_iam_member.deployment_alerting]

  lifecycle {
    precondition {
      condition     = !var.create_alerts || length(var.notification_channel_ids) > 0
      error_message = "notification_channel_ids must be set when create_alerts is enabled"
    }
  }

  conditions {
    display_name = "Cloud Run logs contain metrics shutdown failed"

    condition_matched_log {
      filter = "resource.type=\"cloud_run_job\" AND severity>=WARNING AND jsonPayload.message=\"metrics shutdown failed\""
    }
  }

  alert_strategy {
    notification_rate_limit {
      period = "300s"
    }
    auto_close = "1800s"
  }

  documentation {
    content   = "The canary completed, but metrics export shutdown failed. Review the Cloud Run Job logs and verify OTLP connectivity before treating the run as healthy."
    mime_type = "text/markdown"
  }

  user_labels = local.labels
}
