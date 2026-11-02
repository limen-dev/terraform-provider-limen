# Written by a person: log the provider in with `limen login`, not as a bot.
resource "limen_bot" "deploy_staging" {
  name = "deploy-staging"

  spec = {
    join = {
      method = "github_actions"
      match = {
        repository = "acme/infra"
        ref        = ["refs/heads/main"]
      }
    }
    roles   = ["deployer"]
    max_ttl = "15m"
  }
}
