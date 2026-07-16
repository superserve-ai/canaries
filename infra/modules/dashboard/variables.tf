variable "project_id" {
  type = string
}

variable "environment" {
  type = string
}

variable "targets" {
  type = list(object({
    target = string
    region = string
  }))
}

variable "dashboard_enabled" {
  type    = bool
  default = false
}
