locals {
  dashboard_ids = {
    for name, dashboard in google_monitoring_dashboard.this :
    name => replace(dashboard.id, "projects/${var.project_id}/dashboards/", "")
  }
  dashboard_urls = {
    for name, dashboard in google_monitoring_dashboard.this :
    name => format(
      "https://console.cloud.google.com/monitoring/dashboards/builder/%s?project=%s",
      basename(dashboard.id),
      var.project_id,
    )
  }
}

output "dashboard_id" {
  description = "Cloud Monitoring dashboard resource IDs"
  value       = local.dashboard_ids
}

output "dashboard_url" {
  description = "Cloud Monitoring dashboard URLs"
  value       = local.dashboard_urls
}
