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

variable "scheduler_time_zone" {
  type    = string
  default = "Etc/UTC"
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
    condition     = contains(["lifecycle", "template", "snapshot", "burst", "dax", "soak"], var.scenario)
    error_message = "scenario must be lifecycle, template, snapshot, burst, dax, or soak"
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

variable "missing_runs_duration" {
  type        = string
  default     = "600s"
  description = "How long the missing-run condition must hold before it pages; a single empty evaluation is not a stalled canary. Cloud Monitoring caps missing_runs_window plus this value at 25h, so longer windows must use 0s"
}

variable "extra_env" {
  type    = map(string)
  default = {}
}

variable "slack_webhook_secret_name" {
  type        = string
  default     = ""
  description = "Secret holding a Slack incoming webhook; the run posts its benchmark summary there"
}

variable "overlap_skip_min_count" {
  type        = number
  default     = 1
  description = "Lock-skipped executions within overlap_skip_window needed to page; 1 pages on every skip"

  validation {
    condition     = var.overlap_skip_min_count >= 1 && floor(var.overlap_skip_min_count) == var.overlap_skip_min_count
    error_message = "overlap_skip_min_count must be a whole number of at least 1."
  }
}

variable "overlap_skip_window" {
  type        = string
  default     = "15m"
  description = "Lookback for counting lock-skipped executions, as a whole number of s, m, or h"

  validation {
    condition     = can(regex("^[0-9]+[smh]$", var.overlap_skip_window))
    error_message = "overlap_skip_window must be a whole number followed by s, m, or h, for example 60m."
  }
}

variable "overlap_skip_page_without_completion" {
  type        = bool
  default     = false
  description = "Add a separate policy that pages when a skip falls within overlap_skip_window and no run completed within missing_runs_window, so a stuck lock holder still pages when skips do not repeat"
}
