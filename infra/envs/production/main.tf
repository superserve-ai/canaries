locals {
  labels = {
    environment = "production"
    managed_by  = "terraform"
  }

  deployment_alerting_roles = toset([
    "roles/logging.configWriter",
    "roles/monitoring.alertPolicyEditor",
    "roles/monitoring.notificationChannelEditor",
  ])

  retain_failed_sandbox     = false
  retain_failed_sandbox_ttl = "2h"

  lifecycle_targets = {
    production-us-west2 = {
      target_region       = "us-west2"
      job_region          = "us-west2"
      api_base_url        = "https://api-usw.superserve.ai"
      preview_domain      = "usw-sandbox.superserve.ai"
      api_key_secret_name = "api-canary-key-production-us-west2"
      otlp_endpoint       = "https://telemetry.googleapis.com"
      vpc_network         = "superserve-production-vpc"
      vpc_subnetwork      = "superserve-usw2-cr-subnet"
      vpc_tags            = ["cr-usw2"]
      vpc_egress          = "PRIVATE_RANGES_ONLY"
    }
  }

  dashboards = {
    canary = {
      display_name = "production canary dashboard"
      definition = templatefile(abspath("${path.module}/../../dashboards/cloud-monitoring/canary-overview.json.tftpl"), {
        project_id               = var.project_id
        environment              = local.labels.environment
        default_region           = "us-east4"
        regions                  = ["us-west2", "us-east4"]
        janitor_job_name_pattern = "api-canary-janitor-production-.*"
      })
    }
  }
}

resource "google_storage_bucket" "locks" {
  project                     = var.project_id
  name                        = "${var.project_id}-api-canary-locks"
  location                    = "US"
  uniform_bucket_level_access = true
  labels                      = local.labels
}

module "lifecycle" {
  for_each = local.lifecycle_targets
  source   = "../../modules/canary_target"

  project_id                = var.project_id
  job_region                = each.value.job_region
  target_name               = each.key
  environment               = "production"
  target_region             = each.value.target_region
  api_base_url              = each.value.api_base_url
  preview_domain            = each.value.preview_domain
  image                     = var.image
  api_key_secret_name       = each.value.api_key_secret_name
  lock_bucket_name          = google_storage_bucket.locks.name
  otlp_metrics_endpoint     = each.value.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  scheduler_enabled         = true
  notification_channel_ids  = var.notification_channel_ids
  labels                    = merge(local.labels, { region = each.value.target_region })
  create_alerts             = var.create_alerts
  vpc_connector             = try(each.value.vpc_connector, null)
  vpc_egress                = try(each.value.vpc_egress, "ALL_TRAFFIC")
  vpc_network               = try(each.value.vpc_network, null)
  vpc_subnetwork            = try(each.value.vpc_subnetwork, null)
  vpc_tags                  = try(each.value.vpc_tags, [])
  depends_on                = [google_project_iam_member.deployment_alerting]
}

module "janitor" {
  for_each = local.lifecycle_targets
  source   = "../../modules/janitor"

  project_id                = var.project_id
  job_region                = each.value.job_region
  target_name               = each.key
  environment               = "production"
  api_base_url              = each.value.api_base_url
  preview_domain            = each.value.preview_domain
  image                     = var.image
  api_key_secret_name       = each.value.api_key_secret_name
  lock_bucket_name          = google_storage_bucket.locks.name
  otlp_metrics_endpoint     = each.value.otlp_endpoint
  retain_failed_sandbox     = local.retain_failed_sandbox
  retain_failed_sandbox_ttl = local.retain_failed_sandbox_ttl
  notification_channel_ids  = var.notification_channel_ids
  labels                    = merge(local.labels, { region = each.value.target_region })
  enable_alerts             = false
  vpc_connector             = try(each.value.vpc_connector, null)
  vpc_egress                = try(each.value.vpc_egress, "ALL_TRAFFIC")
  vpc_network               = try(each.value.vpc_network, null)
  vpc_subnetwork            = try(each.value.vpc_subnetwork, null)
  vpc_tags                  = try(each.value.vpc_tags, [])
  depends_on                = [google_project_iam_member.deployment_alerting]
}

module "dashboard" {
  source = "../../modules/dashboard"

  project_id = var.project_id
  dashboards = local.dashboards
}

module "permissions" {
  for_each = local.lifecycle_targets
  source   = "../../modules/permissions"

  project_id                              = var.project_id
  lock_bucket_name                        = google_storage_bucket.locks.name
  lifecycle_runtime_service_account_email = module.lifecycle[each.key].runtime_service_account_email
  janitor_runtime_service_account_email   = module.janitor[each.key].runtime_service_account_email
}

resource "google_project_iam_member" "deployment_alerting" {
  for_each = local.deployment_alerting_roles

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${var.deployment_service_account_email}"
}
