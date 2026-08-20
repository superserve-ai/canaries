# SQL-based Monitoring alert conditions query log views through a linked
# BigQuery dataset. The automatically-created _Default bucket exists already;
# this resource acquires it and enables Observability Analytics in place.
resource "google_logging_project_bucket_config" "alert_sql" {
  project          = var.project_id
  location         = "global"
  bucket_id        = "_Default"
  enable_analytics = true

  depends_on = [google_project_iam_member.deployment_alerting]
}

# A log bucket can have at most one linked dataset. Keep this project-level
# prerequisite in the environment root rather than the per-target module.
resource "google_logging_linked_dataset" "alert_sql" {
  parent      = "projects/${var.project_id}"
  location    = "global"
  bucket      = google_logging_project_bucket_config.alert_sql.id
  link_id     = "canary_alerts"
  description = "Linked dataset used by API canary SQL alert policies"
}
