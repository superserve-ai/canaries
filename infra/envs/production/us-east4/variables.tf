variable "project_id" {
  type    = string
  default = "rayai-prod"
}

variable "job_region" {
  type    = string
  default = "us-east4"
}

variable "image" {
  type = string
}

variable "load_runner_image" {
  type        = string
  description = "Image containing the guarded lifecycle load runner, pinned independently of API canary rollbacks."
}

variable "deployment_service_account_email" {
  type = string
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

variable "daily_load_scheduler_enabled" {
  description = "Enable the daily lifecycle load schedule after its rollout prerequisites are verified."
  type        = bool
  default     = true
}

variable "oncall_notification_channel_ids" {
  description = "Existing on-call channels for daily load failures; benchmark summaries use a separate webhook."
  type        = list(string)
  default     = ["projects/rayai-prod/notificationChannels/7690949645937454026"]
}
