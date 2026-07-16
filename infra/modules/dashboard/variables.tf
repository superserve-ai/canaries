variable "project_id" {
  type = string
}

variable "dashboards" {
  type = map(object({
    display_name = string
    definition   = string
  }))
}
