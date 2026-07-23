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

variable "notification_channel_ids" {
  type    = list(string)
  default = []
}
variable "create_alerts" {
  description = "Whether to create monitoring alert policies"
  type        = bool
  default     = true
}
