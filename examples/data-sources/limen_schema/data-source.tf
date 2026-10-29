data "limen_schema" "resource" {
  kind = "resource"
}

# The fields a resource spec accepts, as the control plane parses it.
output "resource_targets" {
  value = keys(jsondecode(data.limen_schema.resource.json_schema).properties)
}
