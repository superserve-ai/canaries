output "dashboard_ids" {
  description = "Cloud Monitoring dashboard resource IDs keyed by logical name."
  value       = module.dashboard.dashboard_ids
}

output "dashboard_urls" {
  description = "Cloud Monitoring dashboard URLs keyed by logical name."
  value       = module.dashboard.dashboard_urls
}

output "alert_sql_linked_dataset_id" {
  description = "Linked dataset ID used by API canary SQL alert policies."
  value       = google_logging_linked_dataset.alert_sql.id
}
