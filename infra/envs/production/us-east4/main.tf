locals {
  labels = {
    environment = "production"
    region      = "us-east4"
    managed_by  = "terraform"
  }

  deployment_alerting_roles = toset([
    "roles/logging.configWriter",
    "roles/monitoring.alertPolicyEditor",
    "roles/monitoring.notificationChannelEditor",
  ])

  otlp_endpoint = "https://telemetry.googleapis.com"

  retain_failed_sandbox     = false
  retain_failed_sandbox_ttl = "2h"

  dashboards = {}
}

resource "google_project_service" "telemetry" {
  project            = var.project_id
  service            = "telemetry.googleapis.com"
  disable_on_destroy = false
}

module "lifecycle" {
  source = "../../../modules/canary_target"

  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "production-us-east4"
  environment               = "production"
  target_region             = "us-east4"
  api_base_url              = "https://api.superserve.ai"
  preview_domain            = "sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = "api-canary-key-production-us-east4"
  lock_bucket_name          = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  scheduler_enabled         = true
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  vpc_connector             = null
  create_alerts             = var.create_alerts
  depends_on = [
    google_project_service.telemetry,
    google_project_iam_member.deployment_alerting,
  ]
}

module "janitor" {
  source = "../../../modules/janitor"

  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "production-us-east4"
  environment               = "production"
  api_base_url              = "https://api.superserve.ai"
  preview_domain            = "sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = "api-canary-key-production-us-east4"
  lock_bucket_name          = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  enable_alerts             = false
  vpc_connector             = null
  create_alerts             = var.create_alerts
  depends_on                = [google_project_service.telemetry, google_project_iam_member.deployment_alerting]
}

module "dashboard" {
  source = "../../../modules/dashboard"

  project_id = var.project_id
  dashboards = local.dashboards
}

module "permissions" {
  source = "../../../modules/permissions"

  project_id                              = var.project_id
  lock_bucket_name                        = "${var.project_id}-api-canary-locks"
  lifecycle_runtime_service_account_email = module.lifecycle.runtime_service_account_email
  janitor_runtime_service_account_email   = module.janitor.runtime_service_account_email
}

resource "google_project_iam_member" "deployment_alerting" {
  for_each = local.deployment_alerting_roles

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${var.deployment_service_account_email}"
}
