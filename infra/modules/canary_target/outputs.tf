output "runtime_service_account_email" {
  description = "Runtime service account email for the lifecycle target."
  value       = var.runtime_service_account_email
}

output "scheduler_service_account_email" {
  description = "Scheduler service account email for the lifecycle target."
  value       = google_service_account.scheduler.email
}
