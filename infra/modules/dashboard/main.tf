locals {
  dashboard_templates = {
    lifecycle = "canary-lifecycle.json.tftpl"
    janitor   = "canary-janitor.json.tftpl"
    cleanup   = "canary-cleanup.json.tftpl"
  }
}

resource "google_monitoring_dashboard" "this" {
  for_each = var.dashboard_enabled ? local.dashboard_templates : {}
  project  = var.project_id
  dashboard_json = templatefile("${path.module}/../../dashboards/cloud-monitoring/${each.value}", {
    project_id  = var.project_id
    environment = var.environment
    targets     = var.targets
  })
}
