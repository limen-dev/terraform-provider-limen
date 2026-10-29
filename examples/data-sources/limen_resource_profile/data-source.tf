# What a module may set when it completes the profile.
data "limen_resource_profile" "payments_postgres" {
  name = "payments-postgres"
}

output "settable_fields" {
  value = keys(data.limen_resource_profile.payments_postgres.spec.settable)
}
