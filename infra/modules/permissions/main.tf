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
