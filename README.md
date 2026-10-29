# terraform-provider-limen

Terraform and OpenTofu provider for [Limen](https://github.com/limen-dev/limen), the self-hosted, identity-aware access proxy.

> **Not started yet.** The provider is a thin client of Limen's management API (`limen.manage.v1`), which doesn't exist yet. Work starts when stage 1 of Limen's Phase 14 lands. Design: [ADR-0042](https://github.com/limen-dev/limen/blob/main/docs/adr/0042-management-api.md). Tracking: [limen-dev/limen#73](https://github.com/limen-dev/limen/issues/73).

## What it will do
A team's Terraform module creates a database and registers it in Limen in the same plan, in its own state:

```hcl
terraform {
  required_providers {
    limen = { source = "limen-dev/limen" }
  }
}

resource "limen_resource" "orders_db" {
  name    = "payments-orders-db"
  profile = "payments-postgres" # written by a person; fixes credentials, worker, and audit settings
  spec = {
    postgres = { endpoint = aws_db_instance.orders.endpoint }
  }
  labels = { env = "staging" }
}
```

Planned, in order:
1. `limen_resource` (resources that complete a resource profile), after Limen's Phase 14 stage 1.
2. `limen_role`, `limen_role_mapping`, and `limen_bot`, after stage 2.
3. Data sources: `limen_resource_access` (who can reach a resource) and `limen_workers`.

## How it will authenticate
As a Limen **bot** (workload identity from GitHub Actions, GitLab CI, or any OIDC issuer) holding a `policy_writer` grant, never `limen:admin`, and with no long-lived secrets. Plans in pull requests run offline, because Limen never lets pull request runs log in as a bot.

## Releases
Written in Go with `terraform-plugin-framework`, with its own semantic version and a stated range of supported Limen servers. It will be published to the Terraform and OpenTofu registries as `limen-dev/limen` once this repository is public. Until then it is installed with `dev_overrides`.

## License
Apache-2.0, the same as Limen.
