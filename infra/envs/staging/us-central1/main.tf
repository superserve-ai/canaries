locals {
  labels = {
    environment = "staging"
    region      = "us-central1"
    managed_by  = "terraform"
  }

  otlp_endpoint             = "http://10.0.0.2:4318"
  retain_failed_sandbox     = true
  retain_failed_sandbox_ttl = "2h"
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
}

module "janitor" {
  source = "../../../modules/janitor"

  project_id                = var.project_id
  job_region                = var.job_region
  target_name               = "staging"
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
}
