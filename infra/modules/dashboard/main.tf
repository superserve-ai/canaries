resource "google_monitoring_dashboard" "this" {
  for_each       = var.dashboards
  project        = var.project_id
  dashboard_json = each.value.definition
}
