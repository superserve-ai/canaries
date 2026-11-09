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

variable "load_operations" {
  description = "Lifecycles per daily run. Size this to what the target's hosts can finish; the default is the production size."
  type        = number
  default     = 1000
  validation {
    condition     = var.load_operations >= 1 && var.load_operations <= 10000 && floor(var.load_operations) == var.load_operations
    error_message = "load_operations must be a whole number between 1 and 10000."
  }
}

variable "load_concurrency" {
  description = "Maximum lifecycles in flight. One uncertain create or failed cleanup cancels every in-flight lifecycle, so this also bounds the blast radius of a halt."
  type        = number
  default     = 100
  validation {
    condition     = var.load_concurrency >= 1 && floor(var.load_concurrency) == var.load_concurrency
    error_message = "load_concurrency must be a positive whole number."
  }
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
