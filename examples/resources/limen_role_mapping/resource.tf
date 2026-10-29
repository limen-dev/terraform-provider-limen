# Widens standing access, so a bot's write is held for a person to approve.
# The plan says so; apply names the change, or waits with approval_timeout.
resource "limen_role_mapping" "payments" {
  name = "payments-staging"

  spec = [
    { idp_group = "okta-payments-eng", roles = [limen_role.payments_read.name] },
  ]
}
