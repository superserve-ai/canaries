mock_provider "google" {}

override_resource {
  override_during = plan
  target          = google_service_account.runtime
  values = {
    name      = "projects/example-project/serviceAccounts/runtime@example-project.iam.gserviceaccount.com"
    email     = "runtime@example-project.iam.gserviceaccount.com"
    unique_id = "123456789012345678901"
  }
}

override_resource {
  override_during = plan
  target          = google_service_account.scheduler
  values = {
    name      = "projects/example-project/serviceAccounts/scheduler@example-project.iam.gserviceaccount.com"
    email     = "scheduler@example-project.iam.gserviceaccount.com"
    unique_id = "223456789012345678901"
  }
}

variables {
  project_id              = "example-project"
  job_region              = "us-central1"
  environment             = "staging"
  target_name             = "example-target"
  image                   = "us-central1-docker.pkg.dev/example-project/canaries/api:example"
  api_key_secret_name     = "example-api-key"
  api_base_url            = "https://api.example.com"
  lock_bucket_name        = "example-locks"
  otlp_metrics_endpoint   = "https://telemetry.example.com"
  vpc_connector           = null
  target_region           = "us-central1"
  console_url             = "https://console.example.com"
  ui_email_secret_name    = "example-ui-email"
  ui_password_secret_name = "test"
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
  assert {
    condition = (
      length(terraform_data.runtime_deploy_act_as_ready) == 1 &&
      length(terraform_data.scheduler_deploy_act_as_ready) == 1 &&
      terraform_data.runtime_deploy_act_as_ready[0].triggers_replace.service_account_unique_id == google_service_account.runtime.unique_id &&
      terraform_data.scheduler_deploy_act_as_ready[0].triggers_replace.service_account_unique_id == google_service_account.scheduler.unique_id &&
      terraform_data.runtime_deploy_act_as_ready[0].triggers_replace.deployment_service_account_email == var.deployment_service_account_email &&
      terraform_data.scheduler_deploy_act_as_ready[0].triggers_replace.deployment_service_account_email == var.deployment_service_account_email
    )
    error_message = "Readiness checks must follow the account generation and configured deployment identity."
  }

}

run "existing_callers_without_principal" {
  command = plan
  assert {
    condition = (
      length(google_service_account_iam_member.runtime_deploy_act_as) == 0 &&
      length(google_service_account_iam_member.scheduler_deploy_act_as) == 0 &&
      length(terraform_data.runtime_deploy_act_as_ready) == 0 &&
      length(terraform_data.scheduler_deploy_act_as_ready) == 0
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
