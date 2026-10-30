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

  # Created by the production root, which applies first.
  benchmark_slack_webhook_secret_name = "canary-benchmark-slack-webhook"
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

module "ui_canary" {
  source = "../../../modules/ui_canary"

  project_id              = var.project_id
  job_region              = var.job_region
  target_name             = "production-us-east4"
  environment             = "production"
  target_region           = "us-east4"
  console_url             = "https://console.superserve.ai"
  sandbox_id_prefix       = "sb-use-"
  team_label              = "superserve.canary@gmail.com · US East"
  image                   = var.ui_canary_image
  scheduler_enabled       = var.ui_scheduler_enabled
  ui_email_secret_name    = "ui-canary-email-production-us-east4"
  ui_password_secret_name = "ui-canary-password-production-us-east4"
  api_key_secret_name     = module.lifecycle.api_key_secret_name
  api_base_url            = "https://api.superserve.ai"
  lock_bucket_name        = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint   = local.otlp_endpoint
  labels                  = local.labels
  vpc_connector           = null
  depends_on              = [google_project_service.telemetry]
}

module "template" {
  source = "../../../modules/canary_target"

  scenario                  = "template"
  create_api_key_secret     = false
  scheduler_cron            = "0 * * * *"
  job_timeout               = "1800s"
  run_timeout               = "25m"
  lock_ttl                  = "30m"
  missing_runs_window       = "2h"
  create_alerts             = var.create_alerts
  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "production-us-east4"
  environment               = "production"
  target_region             = "us-east4"
  api_base_url              = "https://api.superserve.ai"
  preview_domain            = "sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = module.lifecycle.api_key_secret_name
  lock_bucket_name          = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  vpc_connector             = null
  depends_on = [
    google_project_service.telemetry,
    google_project_iam_member.deployment_alerting,
  ]
}

module "snapshot" {
  source = "../../../modules/canary_target"

  scenario                  = "snapshot"
  create_api_key_secret     = false
  scheduler_cron            = "*/15 * * * *"
  job_timeout               = "900s"
  run_timeout               = "10m"
  lock_ttl                  = "15m"
  missing_runs_window       = "45m"
  create_alerts             = var.create_alerts
  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "production-us-east4"
  environment               = "production"
  target_region             = "us-east4"
  api_base_url              = "https://api.superserve.ai"
  preview_domain            = "sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = module.lifecycle.api_key_secret_name
  lock_bucket_name          = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  vpc_connector             = null
  depends_on = [
    google_project_service.telemetry,
    google_project_iam_member.deployment_alerting,
  ]
}

module "burst" {
  source = "../../../modules/canary_target"

  scenario                  = "burst"
  create_api_key_secret     = false
  scheduler_cron            = "0 12 * * *"
  scheduler_time_zone       = "America/Los_Angeles"
  job_timeout               = "900s"
  run_timeout               = "10m"
  lock_ttl                  = "15m"
  missing_runs_window       = "27h"
  missing_runs_duration     = "0s" # lookback plus retest window must be at most 25h
  extra_env                 = { HTTP_TIMEOUT = "2m" }
  slack_webhook_secret_name = local.benchmark_slack_webhook_secret_name
  create_alerts             = var.create_alerts
  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "production-us-east4"
  environment               = "production"
  target_region             = "us-east4"
  api_base_url              = "https://api.superserve.ai"
  preview_domain            = "sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = module.lifecycle.api_key_secret_name
  lock_bucket_name          = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  vpc_connector             = null
  depends_on = [
    google_project_service.telemetry,
    google_project_iam_member.deployment_alerting,
  ]
}

module "dax" {
  source = "../../../modules/canary_target"

  scenario                  = "dax"
  create_api_key_secret     = false
  scheduler_cron            = "30 12 * * *"
  scheduler_time_zone       = "America/Los_Angeles"
  job_timeout               = "2700s"
  run_timeout               = "40m"
  lock_ttl                  = "45m"
  missing_runs_window       = "27h"
  missing_runs_duration     = "0s" # lookback plus retest window must be at most 25h
  extra_env                 = { HTTP_TIMEOUT = "11m" }
  slack_webhook_secret_name = local.benchmark_slack_webhook_secret_name
  create_alerts             = var.create_alerts
  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "production-us-east4"
  environment               = "production"
  target_region             = "us-east4"
  api_base_url              = "https://api.superserve.ai"
  preview_domain            = "sandbox.superserve.ai"
  image                     = var.image
  api_key_secret_name       = module.lifecycle.api_key_secret_name
  lock_bucket_name          = "${var.project_id}-api-canary-locks"
  otlp_metrics_endpoint     = local.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = local.labels
  vpc_connector             = null
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
  template_runtime_service_account_email  = module.template.runtime_service_account_email
  snapshot_runtime_service_account_email  = module.snapshot.runtime_service_account_email
  benchmark_runtime_service_account_emails = {
    burst = module.burst.runtime_service_account_email
    dax   = module.dax.runtime_service_account_email
  }
}

resource "google_project_iam_member" "deployment_alerting" {
  for_each = local.deployment_alerting_roles

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${var.deployment_service_account_email}"
}
