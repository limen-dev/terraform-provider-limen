# Terraform and OpenTofu provider for Limen

[![ci-go](https://github.com/limen-dev/terraform-provider-limen/actions/workflows/ci-go.yml/badge.svg)](https://github.com/limen-dev/terraform-provider-limen/actions/workflows/ci-go.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Manage [Limen](https://github.com/limen-dev/limen), the self-hosted, identity-aware access proxy, as code. A team's module creates a database and registers it in Limen **in the same apply, in its own state**. The pipeline logs in as a Limen bot with its CI token: no admin rights and no stored secret.

```hcl
resource "limen_resource" "orders_db" {
  name    = "payments-orders-db"
  profile = "payments-postgres" # written by a person: credentials, IAM, worker, and audit are fixed there
  spec = {
    postgres = { endpoint = "${aws_db_instance.orders.address}:5432" }
  }
  labels = { env = "staging" }
}
```

> [!IMPORTANT]
> **Status: checked against Limen's Phase 14 control plane, not yet released.** The provider implements `limen.manage.v1` ([ADR-0042](https://github.com/limen-dev/limen/blob/main/docs/adr/0042-management-api.md), [`manage.proto`](proto/limen/manage/v1/manage.proto)). `TestLiveAdminLifecycle` runs seven kinds of document (every kind but `discovery`, which was applied, planned, and destroyed by hand) in one apply, then an empty second plan, an update, an import, and a destroy against a real control plane (Limen `main` at `fc6495b`, the demo stack), and a bot's held change was approved from the CLI and picked up by the next apply. CI still runs only against [`internal/fakelimen`](internal/fakelimen). Not yet checked: real GitHub tokens (the demo's issuer is a stand-in), `--widening-approval=all`, and a Limen behind a load balancer.

## Usage

```hcl
terraform {
  required_providers {
    limen = { source = "limen-dev/limen" }
  }
}

provider "limen" {
  address        = "limen.example.com:443"
  ca_file        = "limen-ca.pem"
  default_source = "terraform/payments-prod" # one per state: who owns what this creates
  bot            = { method = "github_actions", name = "payments-terraform" }
}
```

A person first grants the bot a `policy_writer` (its namespace, name patterns, and the resource profiles it may complete). The [GitHub Actions guide](docs/guides/github-actions.md) has the grant and the workflow. From a laptop, run `limen login` and drop the `bot` block.

## What it manages

| | |
|---|---|
| **Resources** | `limen_resource`, `limen_role`, `limen_role_mapping`, `limen_policy_test`, and the people-only `limen_bot`, `limen_resource_profile`, `limen_policy_writer`, `limen_discovery` |
| **Data sources** | `limen_<kind>` for every kind, `limen_documents`, `limen_caller`, `limen_schema` |

`bot`, `resource_profile`, `policy_writer`, and `discovery` can be written only by **people**, so a pipeline can't widen its own grant: manage them from a state whose provider is logged in as a person (`limen login`, or a client certificate). A bot is refused with a message saying so. See the [platform and team states guide](docs/guides/split-states.md). A `spec` has exactly the shape of the YAML spec, so `yamldecode(file("role.yaml"))` works. Full reference: [`docs/`](docs/index.md).

## How it behaves

- **Plans are dry runs.** Limen checks every change during plan (grant, validation, profile bounds), refusals fail the plan, and who gains or loses access is shown beside the diff.
- **Nothing is overwritten.** Writes carry the etag, and a document another channel (Git, the console) or another state owns is never touched; the error names its owner.
- **No phantom diffs.** Defaults Limen leaves out of a spec never show as changes.
- **Approval when access widens.** A bot's widening change waits for a person; apply names it, or waits with `approval_timeout`.
- **Safe retries.** Every write has a request id, so retries never apply twice.
- **Pull requests plan offline** with `offline = true`, since PR runs can never log in as a bot.

## Authentication

| | |
|---|---|
| **Bot** | `github_actions`, `gitlab`, or any `oidc` issuer. No stored secret; the certificate lasts at most an hour and is renewed when the platform can mint another token. |
| **Client certificate** | `client_certificate = { cert_pem, key_pem }` |
| **Limen CLI login** | Nothing to configure after `limen login`. |

Every setting has a `LIMEN_*` environment variable. A bot's token works once: the provider logs in once per run, so a plan and an apply in separate runs each need their own token (GitHub Actions mints one per request).

## Compatibility

| Provider | Limen | Terraform | OpenTofu |
|---|---|---|---|
| 0.x | Phase 14 (every document kind) | 1.5+ | 1.6+ |

CI runs the acceptance tests on the oldest and newest of each CLI against [`internal/fakelimen`](internal/fakelimen), an in-memory stand-in for the API. Create, update, delete, import, held changes, and plan-time dry runs were verified by hand against a real Phase 14 control plane (see the status note above).

## Development

```sh
mise install        # Go, golangci-lint, buf, goreleaser, terraform, tofu
make test           # unit tests
make testacc        # acceptance tests: real terraform against fakelimen (make testacc-tofu for tofu)
make fake           # run the in-memory Limen to try the provider by hand
make generate       # Go code from proto/, docs/ from the schema and examples/
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the layout and how the API contract stays in sync with Limen. Pull requests start as drafts, with conventional commits.

## More

[Releasing](RELEASING.md) · [Security](SECURITY.md) · [Agents](AGENTS.md) · [Apache-2.0](LICENSE)
