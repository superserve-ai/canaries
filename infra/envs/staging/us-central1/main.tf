locals {
  labels = {
    environment = "staging"
    region      = "us-central1"
    managed_by  = "terraform"
  }

  deployer_service_account_id = "superserve-canary-deployer"

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

resource "google_service_account" "deployer" {
  project      = var.project_id
  account_id   = local.deployer_service_account_id
  display_name = "Superserve Canary Deployer (staging)"
}

resource "google_service_account" "runner" {
  project      = var.project_id
  account_id   = "apicn-staging-us-central1"
  display_name = "API Canary staging-us-central1"
}

resource "google_storage_bucket_iam_member" "terraform_state_deployer" {
  bucket = "superserve-terraform-state"
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.deployer.email}"
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

  project_id                     = var.project_id
  job_region                     = var.job_region
  target_name                    = "staging-us-central1"
  environment                    = "staging"
  target_region                  = "us-central1"
  api_base_url                   = "https://api-staging.superserve.ai"
  preview_domain                 = "staging-sandbox.superserve.ai"
  image                          = var.image
  api_key_secret_name            = "api-canary-key-staging-us-central1"
  lock_bucket_name               = google_storage_bucket.locks.name
  otlp_metrics_endpoint          = local.otlp_endpoint
  retain_failed_sandbox          = local.retain_failed_sandbox
  retain_failed_sandbox_ttl      = local.retain_failed_sandbox_ttl
  manual_staging_opt_in          = true
  notification_channel_ids       = var.notification_channel_ids
  labels                         = local.labels
  create_alerts                  = var.create_alerts
  runtime_service_account_email  = google_service_account.runner.email
  deployer_service_account_email = google_service_account.deployer.email
  vpc_connector                  = "projects/rayai-dev/locations/us-central1/connectors/ss-vpc-conn-f1b3552"
  vpc_egress                     = "ALL_TRAFFIC"
  depends_on = [
    google_service_account_iam_member.runner_user,
    google_project_iam_member.deployer_artifact_registry_writer,
    google_project_iam_member.deployer_cloud_scheduler_admin,
    google_project_iam_member.deployer_iam_admin,
    google_project_iam_member.deployer_logging_config_writer,
    google_project_iam_member.deployer_monitoring_alert_policy_editor,
    google_project_iam_member.deployer_monitoring_notification_channel_editor,
    google_project_iam_member.deployer_monitoring_dashboard_editor,
    google_project_iam_member.deployer_resourcemanager_project_iam_admin,
    google_project_iam_member.deployer_run_admin,
    google_project_iam_member.deployer_secret_manager_admin,
    google_project_iam_member.deployer_service_usage_admin,
    google_project_iam_member.deployer_storage_admin,
  ]
}

module "janitor" {
  source = "../../../modules/janitor"

  project_id                    = var.project_id
  job_region                    = var.job_region
  target_name                   = "staging-us-central1"
  environment                   = "staging"
  api_base_url                  = "https://api-staging.superserve.ai"
  preview_domain                = "staging-sandbox.superserve.ai"
  image                         = var.image
  api_key_secret_name           = "api-canary-key-staging-us-central1"
  lock_bucket_name              = google_storage_bucket.locks.name
  otlp_metrics_endpoint         = local.otlp_endpoint
  retain_failed_sandbox         = local.retain_failed_sandbox
  retain_failed_sandbox_ttl     = local.retain_failed_sandbox_ttl
  notification_channel_ids      = var.notification_channel_ids
  labels                        = local.labels
  create_alerts                 = var.create_alerts
  runtime_service_account_email = google_service_account.runner.email
  vpc_connector                 = "projects/rayai-dev/locations/us-central1/connectors/ss-vpc-conn-f1b3552"
  vpc_egress                    = "ALL_TRAFFIC"
  depends_on = [
    google_service_account_iam_member.runner_user,
    google_project_iam_member.deployer_artifact_registry_writer,
    google_project_iam_member.deployer_cloud_scheduler_admin,
    google_project_iam_member.deployer_iam_admin,
    google_project_iam_member.deployer_logging_config_writer,
    google_project_iam_member.deployer_monitoring_alert_policy_editor,
    google_project_iam_member.deployer_monitoring_notification_channel_editor,
    google_project_iam_member.deployer_monitoring_dashboard_editor,
    google_project_iam_member.deployer_resourcemanager_project_iam_admin,
    google_project_iam_member.deployer_run_admin,
    google_project_iam_member.deployer_secret_manager_admin,
    google_project_iam_member.deployer_service_usage_admin,
    google_project_iam_member.deployer_storage_admin,
  ]
}

module "dashboard" {
  source = "../../../modules/dashboard"

  project_id = var.project_id
  dashboards = local.dashboards
}

module "permissions" {
  source = "../../../modules/permissions"

  project_id                    = var.project_id
  lock_bucket_name              = google_storage_bucket.locks.name
  runtime_service_account_email = google_service_account.runner.email
}

resource "google_service_account_iam_member" "runner_user" {
  service_account_id = google_service_account.runner.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_artifact_registry_writer" {
  project = var.project_id
  role    = "roles/artifactregistry.writer"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_cloud_scheduler_admin" {
  project = var.project_id
  role    = "roles/cloudscheduler.admin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_iam_admin" {
  project = var.project_id
  role    = "roles/iam.serviceAccountAdmin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_logging_config_writer" {
  project = var.project_id
  role    = "roles/logging.configWriter"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_monitoring_alert_policy_editor" {
  project = var.project_id
  role    = "roles/monitoring.alertPolicyEditor"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_monitoring_notification_channel_editor" {
  project = var.project_id
  role    = "roles/monitoring.notificationChannelEditor"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_monitoring_dashboard_editor" {
  project = var.project_id
  role    = "roles/monitoring.dashboardEditor"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_resourcemanager_project_iam_admin" {
  project = var.project_id
  role    = "roles/resourcemanager.projectIamAdmin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_run_admin" {
  project = var.project_id
  role    = "roles/run.admin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_secret_manager_admin" {
  project = var.project_id
  role    = "roles/secretmanager.admin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_service_usage_admin" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageAdmin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer_storage_admin" {
  project = var.project_id
  role    = "roles/storage.admin"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

moved {
  from = module.lifecycle.google_service_account.runtime
  to   = google_service_account.runner
}

resource "google_monitoring_alert_policy" "metrics_shutdown_failed" {
  count                 = var.create_alerts ? 1 : 0
  project               = var.project_id
  display_name          = "staging API canary: metrics shutdown failed"
  combiner              = "OR"
  enabled               = true
  notification_channels = var.notification_channel_ids
  depends_on            = [google_project_iam_member.deployer_logging_config_writer, google_project_iam_member.deployer_monitoring_alert_policy_editor, google_project_iam_member.deployer_monitoring_notification_channel_editor]

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
