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

variable "labels" {
  type    = map(string)
  default = {}
}

variable "notification_channel_ids" {
  type    = list(string)
  default = []
}
