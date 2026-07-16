output "dashboards" {
  description = "Created Cloud Monitoring dashboards keyed by logical name."
  value = {
    for name, dashboard in google_monitoring_dashboard.this :
    name => {
      display_name = var.dashboards[name].display_name
      id           = replace(dashboard.id, "projects/${var.project_id}/dashboards/", "")
      url = format(
        "https://console.cloud.google.com/monitoring/dashboards/builder/%s?project=%s",
        basename(dashboard.id),
        var.project_id,
      )
    }
  }
}

output "dashboard_ids" {
  description = "Created Cloud Monitoring dashboard IDs keyed by logical name."
  value = {
    for name, dashboard in google_monitoring_dashboard.this :
    name => replace(dashboard.id, "projects/${var.project_id}/dashboards/", "")
  }
}

output "dashboard_urls" {
  description = "Created Cloud Monitoring dashboard URLs keyed by logical name."
  value = {
    for name, dashboard in google_monitoring_dashboard.this :
    name => format(
      "https://console.cloud.google.com/monitoring/dashboards/builder/%s?project=%s",
      basename(dashboard.id),
      var.project_id,
    )
  }
}
