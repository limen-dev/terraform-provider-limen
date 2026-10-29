data "limen_caller" "me" {}

# Fail early, with a clear message, when the bot's grant doesn't cover the
# profile this module needs.
resource "terraform_data" "grant_check" {
  lifecycle {
    precondition {
      condition     = contains(data.limen_caller.me.grant.profiles, "payments-postgres")
      error_message = "${data.limen_caller.me.principal} may not complete the payments-postgres profile."
    }
  }
}
