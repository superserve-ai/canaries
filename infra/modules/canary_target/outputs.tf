output "runtime_service_account_email" {
  description = "Runtime service account email for the lifecycle target."
  value       = google_service_account.runtime.email
}
