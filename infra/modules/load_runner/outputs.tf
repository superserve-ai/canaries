output "job_name" { value = google_cloud_run_v2_job.daily.name }
output "scheduler_job_name" { value = google_cloud_scheduler_job.daily.name }
output "runtime_service_account_email" { value = local.runtime_email }
