resource "google_storage_bucket_iam_member" "lifecycle_lock_admin" {
  bucket = var.lock_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${var.lifecycle_runtime_service_account_email}"
}

resource "google_project_iam_member" "lifecycle_metrics_writer" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${var.lifecycle_runtime_service_account_email}"
}

resource "google_project_iam_member" "lifecycle_service_usage_consumer" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${var.lifecycle_runtime_service_account_email}"
}

resource "google_project_iam_member" "janitor_metrics_writer" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${var.janitor_runtime_service_account_email}"
}

resource "google_project_iam_member" "janitor_service_usage_consumer" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${var.janitor_runtime_service_account_email}"
}

resource "google_storage_bucket_iam_member" "template_lock_admin" {
  count  = var.template_runtime_service_account_email != "" ? 1 : 0
  bucket = var.lock_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${var.template_runtime_service_account_email}"
}

resource "google_project_iam_member" "template_metrics_writer" {
  count   = var.template_runtime_service_account_email != "" ? 1 : 0
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${var.template_runtime_service_account_email}"
}

resource "google_project_iam_member" "template_service_usage_consumer" {
  count   = var.template_runtime_service_account_email != "" ? 1 : 0
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${var.template_runtime_service_account_email}"
}

resource "google_storage_bucket_iam_member" "snapshot_lock_admin" {
  count  = var.snapshot_runtime_service_account_email != "" ? 1 : 0
  bucket = var.lock_bucket_name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${var.snapshot_runtime_service_account_email}"
}

resource "google_project_iam_member" "snapshot_metrics_writer" {
  count   = var.snapshot_runtime_service_account_email != "" ? 1 : 0
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${var.snapshot_runtime_service_account_email}"
}

resource "google_project_iam_member" "snapshot_service_usage_consumer" {
  count   = var.snapshot_runtime_service_account_email != "" ? 1 : 0
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${var.snapshot_runtime_service_account_email}"
}
