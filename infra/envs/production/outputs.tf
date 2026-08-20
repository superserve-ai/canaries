output "dashboard_ids" {
  description = "Cloud Monitoring dashboard resource IDs keyed by logical name."
  value       = module.dashboard.dashboard_ids
}

output "dashboard_urls" {
  description = "Cloud Monitoring dashboard URLs keyed by logical name."
  value       = module.dashboard.dashboard_urls
}
