mock_provider "google" {}

override_resource {
  override_during = plan
  target          = google_service_account.runtime
  values = {
    name  = "projects/example-project/serviceAccounts/runtime@example-project.iam.gserviceaccount.com"
    email = "runtime@example-project.iam.gserviceaccount.com"
  }
}

override_resource {
  override_during = plan
  target          = google_service_account.scheduler
  values = {
    name  = "projects/example-project/serviceAccounts/scheduler@example-project.iam.gserviceaccount.com"
    email = "scheduler@example-project.iam.gserviceaccount.com"
  }
}

variables {
  project_id            = "example-project"
  job_region            = "us-central1"
  environment           = "staging"
  target_name           = "example-target"
  image                 = "us-central1-docker.pkg.dev/example-project/canaries/api:example"
  api_key_secret_name   = "example-api-key"
  api_base_url          = "https://api.example.com"
  lock_bucket_name      = "example-locks"
  otlp_metrics_endpoint = "https://telemetry.example.com"
  vpc_connector         = null
  preview_domain        = "preview.example.com"
}

run "scoped_deployment_access" {
  command = plan
  variables {
    deployment_service_account_email = "deploy@example-project.iam.gserviceaccount.com"
  }
  assert {
    condition = (
      google_service_account_iam_member.runtime_deploy_act_as[0].service_account_id == google_service_account.runtime.name &&
      google_service_account_iam_member.runtime_deploy_act_as[0].role == "roles/iam.serviceAccountUser" &&
      google_service_account_iam_member.runtime_deploy_act_as[0].member == "serviceAccount:deploy@example-project.iam.gserviceaccount.com"
    )
    error_message = "Deployment access must be granted only on the runtime account to the configured deployment identity."
  }
  assert {
    condition = (
      google_service_account_iam_member.scheduler_deploy_act_as[0].service_account_id == google_service_account.scheduler.name &&
      google_service_account_iam_member.scheduler_deploy_act_as[0].role == "roles/iam.serviceAccountUser" &&
      google_service_account_iam_member.scheduler_deploy_act_as[0].member == "serviceAccount:deploy@example-project.iam.gserviceaccount.com"
    )
    error_message = "Deployment access must also be scoped to the scheduler identity used by OAuth jobs."
  }
}

run "existing_callers_without_principal" {
  command = plan
  assert {
    condition = (
      length(google_service_account_iam_member.runtime_deploy_act_as) == 0 &&
      length(google_service_account_iam_member.scheduler_deploy_act_as) == 0
    )
    error_message = "Unconfigured callers must not create empty-principal IAM grants."
  }
}

run "reject_empty_deployment_identity" {
  command = plan
  variables {
    deployment_service_account_email = ""
  }
  expect_failures = [var.deployment_service_account_email]
}
