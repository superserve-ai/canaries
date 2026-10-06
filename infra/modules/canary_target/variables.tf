variable "project_id" {
  type = string
}

variable "job_region" {
  type = string
}

variable "target_name" {
  type = string
}

variable "environment" {
  type = string
}

variable "target_region" {
  type = string
}

variable "api_base_url" {
  type = string
}

variable "preview_domain" {
  type = string
}

variable "image" {
  type = string
}

variable "api_key_secret_name" {
  type = string
}

variable "lock_bucket_name" {
  type = string
}

variable "otlp_metrics_endpoint" {
  type = string
}

variable "retain_failed_sandbox" {
  type    = bool
  default = false
}

variable "retain_failed_sandbox_ttl" {
  type    = string
  default = "2h"
}

variable "labels" {
  type    = map(string)
  default = {}
}

variable "scheduler_cron" {
  type    = string
  default = "*/5 * * * *"
}

variable "scheduler_enabled" {
  type    = bool
  default = true
}

variable "manual_staging_opt_in" {
  type    = bool
  default = false
}

variable "notification_channel_ids" {
  type    = list(string)
  default = []
}

variable "create_alerts" {
  type    = bool
  default = false
}

variable "vpc_connector" {
  type = string
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

variable "scenario" {
  type    = string
  default = "lifecycle"

  validation {
    condition     = contains(["lifecycle", "template"], var.scenario)
    error_message = "scenario must be lifecycle or template"
  }
}

variable "create_api_key_secret" {
  type    = bool
  default = true
}

variable "job_timeout" {
  type    = string
  default = "600s"
}

variable "run_timeout" {
  type    = string
  default = "4m"
}

variable "lock_ttl" {
  type    = string
  default = "10m"
}

variable "missing_runs_window" {
  type    = string
  default = "15m"
}
