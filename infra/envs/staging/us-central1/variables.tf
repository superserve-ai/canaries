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
