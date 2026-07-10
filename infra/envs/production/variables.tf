variable "project_id" {
  type    = string
  default = "rayai-prod"
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
