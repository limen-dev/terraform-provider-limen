# The fixed half of a resource. A bot completes only the settable fields.
resource "limen_resource_profile" "payments_postgres" {
  name = "payments-postgres"

  spec = {
    template = {
      postgres        = { iam_auth = true, region = "eu-west-1", tls_ca_file = "/etc/limen/rds-ca.pem" }
      worker_selector = { network = "payments-vpc" }
    }
    settable = {
      "postgres.endpoint" = {
        hosts = ["*.c9xk2p.eu-west-1.rds.amazonaws.com"]
        ports = [5432]
      }
      "postgres.default_database" = {}
    }
    labels = {
      fixed   = { team = "payments" }
      allowed = { env = ["dev", "staging"] }
    }
  }
}
