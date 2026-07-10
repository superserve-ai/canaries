locals {
  labels = {
    environment = "production"
    managed_by  = "terraform"
  }

  lifecycle_targets = {
    production-us-central1 = {
      target_region       = "us-central1"
      api_base_url        = "https://usc-api.superserve.ai"
      preview_domain      = "usc-sandbox.superserve.ai"
      api_key_secret_name = "api-canary-key-production-us-central1"
    }
    production-us-west2 = {
      target_region       = "us-west2"
      api_base_url        = "https://usw-api.superserve.ai"
      preview_domain      = "usw-sandbox.superserve.ai"
      api_key_secret_name = "api-canary-key-production-us-west2"
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

  project_id               = var.project_id
  job_region               = var.job_region
  target_name              = each.key
  environment              = "production"
  target_region            = each.value.target_region
  api_base_url             = each.value.api_base_url
  preview_domain           = each.value.preview_domain
  image                    = var.image
  api_key_secret_name      = each.value.api_key_secret_name
  lock_bucket_name         = google_storage_bucket.locks.name
  notification_channel_ids = var.notification_channel_ids
  labels                   = merge(local.labels, { region = each.value.target_region })
}

module "janitor" {
  source = "../../modules/janitor"

  project_id               = var.project_id
  job_region               = var.job_region
  target_name              = "production"
  environment              = "production"
  api_base_url             = "https://usc-api.superserve.ai"
  preview_domain           = "usc-sandbox.superserve.ai"
  image                    = var.image
  api_key_secret_name      = "api-canary-key-production-us-central1"
  notification_channel_ids = var.notification_channel_ids
  labels                   = local.labels
}
