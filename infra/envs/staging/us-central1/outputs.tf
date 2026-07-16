output "dashboard_id" {
  description = "Cloud Monitoring dashboard resource ID"
  value       = module.dashboard.dashboard_id
}

output "dashboard_url" {
  value = module.dashboard.dashboard_url
}
