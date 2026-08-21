output "runtime_service_account_email" {
  description = "Runtime service account email for the lifecycle target."
  value       = google_service_account.runtime.email
}

output "api_key_secret_name" {
  description = "Secret containing the API key used by the lifecycle runtime."
  value       = google_secret_manager_secret.api_key.secret_id
}
