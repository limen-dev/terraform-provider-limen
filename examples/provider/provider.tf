terraform {
  required_providers {
    limen = {
      source  = "limen-dev/limen"
      version = "0.1.0-rc.1" # release candidates are pinned exactly
    }
  }
}

# In GitHub Actions: log in as the bot with the job's OIDC token. The job
# needs `permissions: id-token: write`; nothing is stored in a secret.
provider "limen" {
  address        = "limen.example.com:443"
  ca_file        = "${path.module}/limen-ca.pem"
  default_source = "terraform/payments-prod" # one source per state

  bot = {
    method = "github_actions"
    name   = "payments-terraform"
  }

  # Pull request runs can never log in as a bot: plan them offline.
  offline = var.pull_request
}

variable "pull_request" {
  type    = bool
  default = false
}
