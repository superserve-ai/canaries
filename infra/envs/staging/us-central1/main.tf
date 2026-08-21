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
      filter = "resource.type=\"cloud_run_job\" AND severity>=WARNING AND textPayload:\"metrics shutdown failed\""
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
