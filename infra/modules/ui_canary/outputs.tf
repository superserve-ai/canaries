output "job_name" {
  value       = google_cloud_run_v2_job.ui_canary.name
  description = "Name of the UI canary Cloud Run Job"
}

output "runtime_service_account_email" {
  value       = google_service_account.runtime.email
  description = "Email of the UI canary runtime service account"
}

output "scheduler_service_account_email" {
  value       = google_service_account.scheduler.email
  description = "Email of the UI canary scheduler service account"
}
