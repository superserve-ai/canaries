variable "project_id" {
  type    = string
  default = "rayai-prod"
}

variable "job_region" {
  type    = string
  default = "us-central1"
}

variable "image" {
  type = string
}

variable "deployment_service_account_email" {
  type     = string
  nullable = false

  validation {
    condition = can(regex(
      "^[a-z0-9-]+@[a-z0-9-]+\\.iam\\.gserviceaccount\\.com$", var.deployment_service_account_email
    ))
    error_message = "deployment_service_account_email must be a service account email."
  }
}

variable "notification_channel_ids" {
  type    = list(string)
  default = []
}

variable "create_alerts" {
  description = "Whether to create monitoring alert policies"
  type        = bool
  default     = true
}

variable "ui_canary_image" {
  type        = string
  description = "Dedicated Playwright UI canary image"
}

variable "ui_scheduler_enabled" {
  type        = bool
  default     = true
  description = "Enable the five-minute UI schedule after credentials and a manual run are verified"
}
