data "limen_resource" "orders" {
  name = "payments-orders-db"
}

output "orders_endpoint" {
  value = data.limen_resource.orders.effective_spec.postgres.endpoint
}
