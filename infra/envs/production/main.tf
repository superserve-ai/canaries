locals {
  labels = {
    environment = "production"
    managed_by  = "terraform"
  }

  deployer_service_account_id = "superserve-canary-deployer"

  deployer_roles = toset([
    "roles/artifactregistry.writer",
    "roles/cloudscheduler.admin",
    "roles/iam.serviceAccountAdmin",
    "roles/logging.configWriter",
    "roles/monitoring.alertPolicyEditor",
    "roles/monitoring.notificationChannelEditor",
    "roles/monitoring.dashboardEditor",
    "roles/resourcemanager.projectIamAdmin",
    "roles/run.admin",
    "roles/secretmanager.admin",
    "roles/serviceusage.serviceUsageAdmin",
    "roles/storage.admin",
  ])

  otlp_endpoints = {
    production-us-central1 = "http://10.0.0.3:4318"
    production-us-west2    = "http://10.1.0.2:4318"
  }

  retain_failed_sandbox     = false
  retain_failed_sandbox_ttl = "2h"

  lifecycle_targets = {
    production-us-central1 = {
      target_region       = "us-central1"
      job_region          = "us-central1"
      api_base_url        = "https://api.superserve.ai"
      preview_domain      = "sandbox.superserve.ai"
      api_key_secret_name = "api-canary-key-production-us-central1"
      otlp_endpoint       = local.otlp_endpoints.production-us-central1
      vpc_connector       = "projects/rayai-prod/locations/us-central1/connectors/superserve-prod-conn"
      vpc_egress          = "PRIVATE_RANGES_ONLY"
    }
    production-us-west2 = {
      target_region       = "us-west2"
      job_region          = "us-west2"
      api_base_url        = "https://usw-api.superserve.ai"
      preview_domain      = "usw-sandbox.superserve.ai"
      api_key_secret_name = "api-canary-key-production-us-west2"
      otlp_endpoint       = local.otlp_endpoints.production-us-west2
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
        default_region           = "us-central1"
        regions                  = ["us-central1", "us-west2", "us-east4"]
        janitor_job_name_pattern = "api-canary-janitor-production-.*"
      })
    }
  }
}

resource "google_service_account" "deployer" {
  project      = var.project_id
  account_id   = local.deployer_service_account_id
  display_name = "Superserve Canary Deployer (production)"
}

resource "google_service_account" "runner" {
  for_each = local.lifecycle_targets

  project      = var.project_id
  account_id   = substr("apicn-${each.key}", 0, 30)
  display_name = "API Canary ${each.key}"
}

resource "google_storage_bucket_iam_member" "terraform_state_deployer" {
  bucket = "superserve-terraform-state-prod"
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.deployer.email}"
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

  project_id                     = var.project_id
  job_region                     = each.value.job_region
  target_name                    = each.key
  environment                    = "production"
  target_region                  = each.value.target_region
  api_base_url                   = each.value.api_base_url
  preview_domain                 = each.value.preview_domain
  image                          = var.image
  api_key_secret_name            = each.value.api_key_secret_name
  lock_bucket_name               = google_storage_bucket.locks.name
  otlp_metrics_endpoint          = each.value.otlp_endpoint
  retain_failed_sandbox          = local.retain_failed_sandbox
  retain_failed_sandbox_ttl      = local.retain_failed_sandbox_ttl
  scheduler_enabled              = true
  notification_channel_ids       = var.notification_channel_ids
  labels                         = merge(local.labels, { region = each.value.target_region })
  create_alerts                  = var.create_alerts
  runtime_service_account_email  = google_service_account.runner[each.key].email
  deployer_service_account_email = google_service_account.deployer.email
  vpc_connector                  = try(each.value.vpc_connector, null)
  vpc_egress                     = try(each.value.vpc_egress, "ALL_TRAFFIC")
  vpc_network                    = try(each.value.vpc_network, null)
  vpc_subnetwork                 = try(each.value.vpc_subnetwork, null)
  vpc_tags                       = try(each.value.vpc_tags, [])
  depends_on = [
    google_service_account_iam_member.runner_user,
    google_project_iam_member.deployer,
  ]
}

module "janitor" {
  for_each = local.lifecycle_targets
  source   = "../../modules/janitor"

  project_id                    = var.project_id
  job_region                    = each.value.job_region
  target_name                   = each.key
  environment                   = "production"
  api_base_url                  = each.value.api_base_url
  preview_domain                = each.value.preview_domain
  image                         = var.image
  api_key_secret_name           = each.value.api_key_secret_name
  lock_bucket_name              = google_storage_bucket.locks.name
  otlp_metrics_endpoint         = each.value.otlp_endpoint
  retain_failed_sandbox         = local.retain_failed_sandbox
  retain_failed_sandbox_ttl     = local.retain_failed_sandbox_ttl
  notification_channel_ids      = var.notification_channel_ids
  labels                        = merge(local.labels, { region = each.value.target_region })
  enable_alerts                 = false
  runtime_service_account_email = google_service_account.runner[each.key].email
  vpc_connector                 = try(each.value.vpc_connector, null)
  vpc_egress                    = try(each.value.vpc_egress, "ALL_TRAFFIC")
  vpc_network                   = try(each.value.vpc_network, null)
  vpc_subnetwork                = try(each.value.vpc_subnetwork, null)
  vpc_tags                      = try(each.value.vpc_tags, [])
  depends_on = [
    google_service_account_iam_member.runner_user,
    google_project_iam_member.deployer,
  ]
}

module "dashboard" {
  source = "../../modules/dashboard"

  project_id = var.project_id
  dashboards = local.dashboards
}

module "permissions" {
  for_each = local.lifecycle_targets
  source   = "../../modules/permissions"

  project_id                    = var.project_id
  lock_bucket_name              = google_storage_bucket.locks.name
  runtime_service_account_email = google_service_account.runner[each.key].email
}

resource "google_service_account_iam_member" "runner_user" {
  for_each = local.lifecycle_targets

  service_account_id = google_service_account.runner[each.key].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_project_iam_member" "deployer" {
  for_each = local.deployer_roles

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

moved {
  from = module.lifecycle["production-us-central1"].google_service_account.runtime
  to   = google_service_account.runner["production-us-central1"]
}

moved {
  from = module.lifecycle["production-us-west2"].google_service_account.runtime
  to   = google_service_account.runner["production-us-west2"]
}
