resource "limen_policy_test" "payments_prod_locked" {
  name = "payments-prod-is-locked-down"

  spec = {
    cases = [
      {
        name      = "payments engineers can't write to prod"
        who       = { groups = ["okta-payments-eng"] }
        resource  = "payments-orders-prod"
        principal = { db_role = "limen_write", database = "orders" }
        expect    = "deny"
      },
    ]
  }
}
