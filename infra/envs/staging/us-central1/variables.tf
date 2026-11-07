variable "project_id" {
  type    = string
  default = "rayai-dev"
}

variable "job_region" {
  type    = string
  default = "us-central1"
}

variable "image" {
  type = string
}

variable "load_runner_image" {
  description = "Image containing the /load-runner binary. Keep this pinned to a load-runner-capable image independently of canary rollbacks."
  type        = string
}

variable "ui_canary_image" {
  type        = string
  description = "Dedicated Playwright UI canary image"
}

variable "ui_scheduler_enabled" {
  description = "Whether the Cloud Scheduler job for UI canary is enabled"
  type        = bool
  default     = true
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

variable "daily_load_scheduler_enabled" {
  description = "Enable the daily lifecycle load schedule after its rollout prerequisites are verified."
  type        = bool
  default     = true
}

variable "oncall_notification_channel_ids" {
  description = "Existing on-call channels for daily load failures; benchmark summaries use a separate webhook."
  type        = list(string)
  default     = ["projects/rayai-dev/notificationChannels/11010951251479934548"]
}
