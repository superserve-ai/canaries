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
