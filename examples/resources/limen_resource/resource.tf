# A team's module creates a database and registers it in Limen in the same
# apply. The profile, written by a person, fixes credentials, IAM, the worker,
# and audit settings; the module sets only the endpoint and a label.
resource "aws_db_instance" "orders" {
  identifier     = "payments-orders"
  engine         = "postgres"
  instance_class = "db.t4g.medium"
  # ...
}

resource "limen_resource" "orders_db" {
  name        = "payments-orders-db"
  description = "Orders, staging"
  profile     = "payments-postgres"

  spec = {
    postgres = {
      endpoint         = "${aws_db_instance.orders.address}:${aws_db_instance.orders.port}"
      default_database = "orders"
    }
  }

  labels = { env = "staging" }
}

# What enforcement uses: the profile's template merged with spec.
output "orders_db_region" {
  value = jsondecode(limen_resource.orders_db.effective_spec_json).postgres.region
}
