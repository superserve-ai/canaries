locals {
  dashboard_resource_name = try(google_monitoring_dashboard.this[0].id, null)
  dashboard_id            = local.dashboard_resource_name == null ? null : replace(local.dashboard_resource_name, "projects/${var.project_id}/dashboards/", "")
}

output "dashboard_id" {
  description = "Cloud Monitoring dashboard resource ID"
  value       = local.dashboard_id
}

output "dashboard_url" {
  description = "Cloud Monitoring dashboard URL"
  value = local.dashboard_resource_name == null ? null : format(
    "https://console.cloud.google.com/monitoring/dashboards/builder/%s?project=%s",
    basename(local.dashboard_resource_name),
    var.project_id,
  )
}
