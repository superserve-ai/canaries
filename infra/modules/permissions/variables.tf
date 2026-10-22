variable "project_id" {
  type = string
}

variable "lock_bucket_name" {
  type = string
}

variable "lifecycle_runtime_service_account_email" {
  type = string
}

variable "janitor_runtime_service_account_email" {
  type = string
}

variable "template_runtime_service_account_email" {
  type    = string
  default = ""
}

variable "snapshot_runtime_service_account_email" {
  type    = string
  default = ""
}

variable "benchmark_runtime_service_account_emails" {
  type        = map(string)
  default     = {}
  description = "Runtime service accounts of the benchmark scenarios, keyed by scenario"
}
