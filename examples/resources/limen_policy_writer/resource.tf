# What the pipeline's bot may write. Only a person can widen it.
resource "limen_policy_writer" "payments_terraform" {
  name = "payments-terraform"

  spec = {
    bot           = limen_bot.payments_terraform.name
    namespace     = "payments"
    names         = ["payments-*"]
    kinds         = ["resource"]
    profiles      = [limen_resource_profile.payments_postgres.name]
    max_documents = 50
    approvers     = { groups = ["security"] }
  }
}
