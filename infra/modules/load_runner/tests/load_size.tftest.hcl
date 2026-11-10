# Plan-only checks of the rendered daily load size. The mock provider needs no
# credentials, so CI can run `terraform test` in this module directory.

mock_provider "google" {}

variables {
  project_id                = "p"
  job_region                = "us-central1"
  target_name               = "t"
  environment               = "production"
  target_region             = "us-east4"
  api_base_url              = "https://x"
  preview_domain            = "x"
  image                     = "img"
  api_key_secret_name       = "k"
  lock_bucket_name          = "b"
  otlp_metrics_endpoint     = "o"
  slack_webhook_secret_name = "s"
  scheduler_cron            = "45 7 * * *"
  daily_window_start        = "07:45"
  daily_window_end          = "08:12"
  create_alerts             = false
}

run "defaults_are_production_size" {
  command = plan
  assert {
    condition     = local.environment_variables.LOAD_TEST_OPERATIONS == "1000" && local.environment_variables.LOAD_TEST_CONCURRENCY == "100"
    error_message = "Production default load size changed."
  }
  assert {
    condition     = google_cloud_scheduler_job.daily.description == "Runs the daily 1000-operation / 100-concurrent lifecycle load for t"
    error_message = "Scheduler description changed: ${google_cloud_scheduler_job.daily.description}"
  }
}

run "staging_size" {
  command = plan
  variables {
    load_operations  = 100
    load_concurrency = 10
  }
  assert {
    condition     = local.environment_variables.LOAD_TEST_OPERATIONS == "100" && local.environment_variables.LOAD_TEST_CONCURRENCY == "10"
    error_message = "load_operations and load_concurrency overrides were not applied."
  }
}

run "concurrency_above_operations_rejected" {
  command = plan
  variables {
    load_operations  = 10
    load_concurrency = 20
  }
  expect_failures = [google_cloud_run_v2_job.daily]
}

run "fractional_rejected" {
  command = plan
  variables {
    load_operations = 10.5
  }
  expect_failures = [var.load_operations]
}
