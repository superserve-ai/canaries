variable "project_id" { type = string }
variable "job_region" { type = string }
variable "target_name" { type = string }
variable "environment" { type = string }
variable "target_region" { type = string }
variable "api_base_url" { type = string }
variable "preview_domain" { type = string }
variable "image" { type = string }
variable "api_key_secret_name" { type = string }
variable "lock_bucket_name" { type = string }
variable "otlp_metrics_endpoint" { type = string }
variable "slack_webhook_secret_name" { type = string }
variable "scheduler_cron" { type = string }
variable "daily_window_start" { type = string }
variable "daily_window_end" { type = string }

variable "scheduler_time_zone" {
  type    = string
  default = "America/Los_Angeles"
}

variable "scheduler_enabled" {
  type    = bool
  default = false
}

variable "create_runtime_service_account" {
  type    = bool
  default = true
}

variable "runtime_service_account_email" {
  type    = string
  default = null
}

variable "manage_api_key_access" {
  type    = bool
  default = true
}

variable "manage_slack_webhook_access" {
  description = "Disable when the secret owner grants cross-project access before deploying this job."
  type        = bool
  default     = true
}

variable "notification_channel_ids" {
  description = "Existing on-call channels; benchmark summary webhooks are configured separately."
  type        = list(string)
  default     = []
}

variable "create_alerts" {
  type    = bool
  default = true
}

variable "labels" {
  type    = map(string)
  default = {}
}

variable "vpc_connector" {
  type    = string
  default = null
}

variable "vpc_egress" {
  type    = string
  default = "ALL_TRAFFIC"
}

variable "vpc_network" {
  type    = string
  default = null
}

variable "vpc_subnetwork" {
  type    = string
  default = null
}

variable "vpc_tags" {
  type    = list(string)
  default = []
}
