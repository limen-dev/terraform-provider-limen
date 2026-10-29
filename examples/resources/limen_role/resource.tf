resource "limen_role" "payments_read" {
  name = "payments-read"

  spec = {
    cert_ttl        = "10m"
    max_session_ttl = "8h"
    allow = [
      {
        resources = { kind = ["postgres"], labels = { team = ["payments"], env = ["staging"] } }
        db        = { roles = ["limen_read"], databases = ["*"] }
      },
    ]
  }
}

# Or keep the role in YAML next to the rest of your policy:
resource "limen_role" "payments_write" {
  name = "payments-write"
  spec = yamldecode(file("${path.module}/roles/payments-write.yaml"))
}
