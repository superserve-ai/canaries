locals {
  dashboard_name = "${var.environment}-canary-observability"
}

resource "google_monitoring_dashboard" "this" {
  count   = var.dashboard_enabled ? 1 : 0
  project = var.project_id
  dashboard_json = templatefile("${path.module}/dashboard.json.tftpl", {
    project_id  = var.project_id
    environment = var.environment
    targets     = var.targets
  })
}
