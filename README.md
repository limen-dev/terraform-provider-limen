# terraform-provider-limen

Terraform and OpenTofu provider for [Limen](https://github.com/limen-dev/limen), the self-hosted, identity-aware access proxy.

A team's Terraform module creates a database and registers it in Limen in the same plan, in its own state, while the security or platform team manages roles and mappings from theirs:

```hcl
terraform {
  required_providers {
    limen = { source = "limen-dev/limen" }
  }
}

provider "limen" {
  address        = "limen.example.com:443"
  ca_file        = "limen-ca.pem"
  default_source = "terraform/payments-prod"
  bot            = { method = "github_actions", name = "payments-terraform" }
}

resource "limen_resource" "orders_db" {
  name    = "payments-orders-db"
  profile = "payments-postgres" # written by a person; fixes credentials, worker, and audit settings
  spec = {
    postgres = { endpoint = "${aws_db_instance.orders.address}:5432" }
  }
  labels = { env = "staging" }
}
```

> **Status.** The provider is built against Limen's management API contract, `limen.manage.v1` ([ADR-0042](https://github.com/limen-dev/limen/blob/main/docs/adr/0042-management-api.md), [`manage.proto`](proto/limen/manage/v1/manage.proto)). The control plane doesn't serve that API yet (Phase 14 stage 1, [limen-dev/limen#73](https://github.com/limen-dev/limen/issues/73)). Until it does, everything here is tested against [`internal/fakelimen`](internal/fakelimen), an in-memory stand-in that serves the same contract, and is **unverified against a real control plane**.

## What it manages

| | |
|---|---|
| Resources | `limen_resource`, `limen_role`, `limen_role_mapping`, `limen_policy_test` |
| Data sources | one per kind (`limen_resource`, `limen_role`, `limen_role_mapping`, `limen_policy_test`, `limen_bot`, `limen_discovery`, `limen_resource_profile`, `limen_policy_writer`), plus `limen_documents` (a filtered list), `limen_caller` (who the provider is and what its grant allows), and `limen_schema` (the JSON Schema of a kind's spec) |

`bot`, `discovery`, `resource_profile`, and `policy_writer` are data sources only: ADR-0042 makes them people-only (written through Git or the console), so no API client, this provider included, can write them. `limen_resource_access` and `limen_workers`, planned in #73, wait for an API that answers them; `limen.manage.v1` doesn't have one.

Each document's `spec` has exactly the shape of its YAML `spec`, as a Terraform value (`spec = yamldecode(file("role.yaml"))` works too). The provider doesn't mirror the spec in its own schema: Limen's Rust types are the single source of truth (ADR-0042 §2), and the control plane validates strictly.

## How it behaves

- **Plans are dry runs.** Every planned change is sent with `validate_only`. Refusals (outside the bot's grant, invalid, owned elsewhere, tests failing) fail the plan, not the apply. Who gains or loses access shows as a warning next to the diff.
- **Nothing is overwritten.** Writes carry the etag, so a change made between plan and apply is refused, never merged. A document another channel or source owns is never written; the error names the owner.
- **No phantom diffs.** Limen returns specs normalized (keys sorted, defaults left out). The state keeps your spelling while Limen's stays the same, so a default you wrote explicitly never shows as a change.
- **Held changes.** A bot's change that widens standing access waits for a person. The plan warns, apply fails naming the change (or waits, with `approval_timeout`), and after approval the next apply finds the document in place.
- **Moving between states.** After `terraform state mv` into another state, that state's `default_source` takes over and the next apply transfers the document.
- **Safe retries.** Every write has a request id, so retries after `UNAVAILABLE` or `RATE_LIMITED` never apply twice.
- **Pull requests plan offline.** Pull request runs can never log in as a bot, so `offline = true` (`LIMEN_OFFLINE`) plans without contacting Limen.

## Authentication

As a Limen **bot** with the CI platform's workload identity token (GitHub Actions, GitLab CI, or any OIDC issuer) holding a `policy_writer` grant, never `limen:admin`, and with no long-lived secrets. The provider logs in again before the one-hour certificate expires when the platform can mint another token. From a laptop, it reuses `limen login`. See [the provider documentation](docs/index.md) and [the GitHub Actions guide](docs/guides/github-actions.md).

## Compatibility

| Provider | Limen | Terraform | OpenTofu |
|---|---|---|---|
| 0.x | a control plane that serves `limen.manage.v1` (Phase 14) | 1.5 and later | 1.6 and later |

CI runs the acceptance tests on the oldest and newest supported version of each CLI.

## Install

Releases are published to the [Terraform Registry](https://registry.terraform.io/providers/limen-dev/limen) and the [OpenTofu Registry](https://search.opentofu.org/provider/limen-dev/limen) as `limen-dev/limen` once this repository is public ([RELEASING.md](RELEASING.md)). Until then, build it and use `dev_overrides` ([CONTRIBUTING.md](CONTRIBUTING.md)).

## Development

```sh
mise install          # Go, golangci-lint, buf, goreleaser, terraform, tofu
make test             # unit tests
make testacc          # acceptance tests with terraform, against the in-memory Limen
make testacc-tofu     # the same with tofu
make fake             # run the in-memory Limen to try the provider by hand
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the layout, how the API contract is kept in sync with Limen, and how to try a local build.

## License

Apache-2.0, the same as Limen.
