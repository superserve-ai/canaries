variable "project_id" {
  type        = string
  description = "GCP Project ID"
}

variable "job_region" {
  type        = string
  description = "Region where the Cloud Run job is deployed"
}

variable "target_name" {
  type        = string
  description = "Target identifier tuple (e.g. staging-us-central1)"
}

variable "environment" {
  type        = string
  description = "Target environment (e.g. staging, production)"
}

variable "target_region" {
  type        = string
  description = "Target region (e.g. us-central1)"
}

variable "console_url" {
  type        = string
  description = "Base URL of the Superserve web console (e.g. https://console-staging.superserve.ai)"
}

variable "image" {
  type        = string
  description = "Container image for the UI canary"
}

variable "ui_email_secret_name" {
  type        = string
  description = "Secret Manager secret ID for the UI canary login email"
}

variable "ui_password_secret_name" {
  type        = string
  description = "Secret Manager secret ID for the UI canary login password"
}

variable "ui_vercel_bypass_secret_name" {
  type        = string
  default     = null
  description = "Optional Secret Manager secret ID for the Vercel deployment protection bypass secret (staging only)"
}

variable "api_key_secret_name" {
  type        = string
  description = "Secret Manager secret ID for the Canary API key (used for durable ownership metadata tagging)"
}

variable "api_base_url" {
  type        = string
  description = "API base URL for tagging sandboxes with ownership metadata"
}

variable "lock_bucket_name" {
  type        = string
  description = "GCS bucket name for target execution locking"
}

variable "otlp_metrics_endpoint" {
  type        = string
  description = "OTLP metrics collector endpoint"
}

variable "labels" {
  type        = map(string)
  default     = {}
  description = "Resource labels to attach to all created infrastructure"
}

variable "scheduler_cron" {
  type        = string
  default     = "*/5 * * * *"
  description = "Cron schedule for the Cloud Scheduler job"
}

variable "scheduler_enabled" {
  type        = bool
  default     = false
  description = "Whether the Cloud Scheduler job is enabled"
}

variable "vpc_connector" {
  type        = string
  default     = null
  description = "VPC connector ID for Cloud Run network access"
}

variable "vpc_egress" {
  type        = string
  default     = "ALL_TRAFFIC"
  description = "VPC egress setting for Cloud Run"
}

variable "vpc_network" {
  type        = string
  default     = null
  description = "VPC network for direct VPC egress"
}

variable "vpc_subnetwork" {
  type        = string
  default     = null
  description = "VPC subnetwork for direct VPC egress"
}

variable "vpc_tags" {
  type        = list(string)
  default     = null
  description = "VPC network tags for direct VPC egress"
}
