# Terraform and OpenTofu provider for Limen

[![ci-go](https://github.com/limen-dev/terraform-provider-limen/actions/workflows/ci-go.yml/badge.svg)](https://github.com/limen-dev/terraform-provider-limen/actions/workflows/ci-go.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Manage [Limen](https://github.com/limen-dev/limen), the self-hosted, identity-aware access proxy, from Terraform or OpenTofu. A team's module creates a database and registers it in Limen **in the same apply, in its own state**, while the security team keeps roles and mappings in theirs. The pipeline logs in as a Limen bot with its CI token: no admin rights, no stored secret.

```hcl
resource "aws_db_instance" "orders" {
  identifier = "payments-orders"
  engine     = "postgres"
  # ...
}

resource "limen_resource" "orders_db" {
  name    = "payments-orders-db"
  profile = "payments-postgres" # written by a person: credentials, worker, IAM, and audit are fixed there
  spec = {
    postgres = { endpoint = "${aws_db_instance.orders.address}:5432" }
  }
  labels = { env = "staging" }
}
```

```text
$ terraform plan
  # limen_resource.orders_db will be created
  ...
Warning: Access changes: resource/payments-orders-db

+ group:payments-eng gains standing access to payments-orders-db as db role app_rw (via role payments-read)
```

> [!IMPORTANT]
> **Status: built ahead of the server.** The provider implements Limen's management API contract, `limen.manage.v1` ([ADR-0042](https://github.com/limen-dev/limen/blob/main/docs/adr/0042-management-api.md), [`manage.proto`](proto/limen/manage/v1/manage.proto)), which the control plane doesn't serve yet ([limen-dev/limen#73](https://github.com/limen-dev/limen/issues/73)). Until it does, it is tested against [`internal/fakelimen`](internal/fakelimen), an in-memory stand-in that serves the same contract, and is **unverified against a real control plane**.

## Contents

- [Quick start](#quick-start)
- [What it manages](#what-it-manages)
- [How it behaves](#how-it-behaves)
- [Authentication](#authentication)
- [Compatibility](#compatibility)
- [Try it without Limen](#try-it-without-limen)
- [Development](#development)
- [Releases](#releases)

## Quick start

**1. A person grants the pipeline's bot what it may write**, in Limen's policy repository:

```yaml
kind: policy_writer
version: v1
metadata: { name: payments-terraform }
spec:
  bot: payments-terraform           # a bot that matches this repository's main branch
  namespace: payments
  names: ["payments-*"]
  kinds: [resource, policy_test]
  profiles: [payments-postgres]
```

**2. Configure the provider** to log in as that bot:

```hcl
terraform {
  required_providers {
    limen = { source = "limen-dev/limen", version = "~> 0.1" }
  }
}

provider "limen" {
  address        = "limen.example.com:443"
  ca_file        = "${path.module}/limen-ca.pem"
  default_source = "terraform/payments-prod" # one per state: which configuration owns what it creates
  bot            = { method = "github_actions", name = "payments-terraform" }
}
```

**3. Run it from CI.** The apply job needs `permissions: id-token: write`. Pull request runs can never log in as a bot, so plan them with `LIMEN_OFFLINE=true`. The [GitHub Actions guide](docs/guides/github-actions.md) has the whole workflow.

From a laptop, `limen login` is enough: without a `bot` block, the provider reuses the CLI's login, address, and CA.

## What it manages

| Resource | What it is |
|---|---|
| [`limen_resource`](docs/resources/resource.md) | Something people connect to: a database, SSH host, cluster, or web app. Bots complete a person's **resource profile**. |
| [`limen_role`](docs/resources/role.md) | Allow and deny rules over resources. |
| [`limen_role_mapping`](docs/resources/role_mapping.md) | Which IdP groups get which roles. |
| [`limen_policy_test`](docs/resources/policy_test.md) | Cases stating who must or must not reach what. |

| Data source | What it reads |
|---|---|
| `limen_<kind>` for every kind | One document, including `limen_bot`, `limen_resource_profile`, `limen_policy_writer`, and `limen_discovery`. |
| [`limen_documents`](docs/data-sources/documents.md) | Documents of one kind, filtered by namespace, source, or labels. |
| [`limen_caller`](docs/data-sources/caller.md) | Who the provider is logged in as, and the bot's grant: handy in preconditions. |
| [`limen_schema`](docs/data-sources/schema.md) | The JSON Schema of a kind's spec. |

A document's `spec` has exactly the shape of its YAML spec, so `spec = yamldecode(file("role.yaml"))` works too. The provider doesn't copy the spec into its own schema: Limen's types are the single source of truth, and the control plane validates strictly.

Not here, on purpose:
- **No `limen_bot`, `limen_resource_profile`, `limen_policy_writer`, or `limen_discovery` resources.** Limen lets only people write those (through Git or the console), so no pipeline can widen its own grant. They are data sources.
- **No `limen_resource_access` or `limen_workers` yet:** the management API has no call that answers them.

## How it behaves

| | |
|---|---|
| **Plans are dry runs** | Every change is checked by Limen during plan (`validate_only`): grant, validation, profile bounds, tests. Refusals fail the plan, not the apply, and who gains or loses access shows next to the diff. |
| **Nothing is overwritten** | Writes carry the document's etag: a change made between plan and apply is refused, never merged. A document another channel (Git, the console) or another state owns is never written; the error names its owner. |
| **No phantom diffs** | Limen stores specs normalized. Your spelling stays in state while Limen's is unchanged, so a default you wrote explicitly never shows as a change. |
| **Approval when access widens** | A bot's change that widens standing access waits for a person. The plan warns; apply names the change, or waits for it with `approval_timeout`. Applying again replaces your earlier request, and after approval the next apply finds the document in place. |
| **Moving between states** | After `terraform state mv`, the new state's `default_source` takes the document over on the next apply. |
| **Safe retries** | Every write has a request id, so retries after `UNAVAILABLE` or rate limiting never apply twice. |
| **Offline plans** | `offline = true` plans without contacting Limen, for pull requests. |

## Authentication

| Method | When | Configure |
|---|---|---|
| Bot, GitHub Actions | CI on GitHub | `bot = { method = "github_actions" }`; the job needs `id-token: write` |
| Bot, GitLab CI | CI on GitLab | `bot = { method = "gitlab" }`; an `id_tokens:` entry `LIMEN_ID_TOKEN` with `aud: limen` |
| Bot, any OIDC issuer | Buildkite, CircleCI, Kubernetes | `bot = { method = "oidc", token_file = "..." }` |
| Client certificate | A certificate you already have | `client_certificate = { cert_pem, key_pem }` |
| The Limen CLI's login | A laptop | nothing: run `limen login` first |

Bot certificates last at most an hour. When the platform can mint another token (GitHub Actions, a token file), the provider logs in again before it expires. Every setting also has an environment variable (`LIMEN_ADDRESS`, `LIMEN_CA_FILE`, `LIMEN_SOURCE`, `LIMEN_BOT_METHOD`, `LIMEN_OFFLINE`, …); see [the provider documentation](docs/index.md).

## Compatibility

| Provider | Limen | Terraform | OpenTofu |
|---|---|---|---|
| 0.x | a control plane that serves `limen.manage.v1` (Phase 14) | 1.5 and later | 1.6 and later |

CI runs the acceptance tests on the oldest and newest supported version of each CLI. The provider uses plugin protocol 6.

## Try it without Limen

`fakelimen` serves the management API from memory, with a bot, a profile, and a fake GitHub token service, so the whole flow runs on a laptop:

```sh
mise install && make install          # builds bin/terraform-provider-limen
make fake &                           # writes .fakelimen/env.sh
source .fakelimen/env.sh
printf 'provider_installation {\n  dev_overrides { "limen-dev/limen" = "%s/bin" }\n  direct {}\n}\n' "$PWD" > dev.tfrc
export TF_CLI_CONFIG_FILE=$PWD/dev.tfrc
```

Then `terraform plan` (or `tofu plan`) in any configuration that uses `limen-dev/limen`. [CONTRIBUTING.md](CONTRIBUTING.md) has the details.

## Development

```sh
mise install          # Go, golangci-lint, buf, goreleaser, terraform, tofu, actionlint
make test             # unit tests
make testacc          # acceptance tests: real terraform against fakelimen
make testacc-tofu     # the same with tofu
make lint             # golangci-lint, buf, actionlint
make generate         # Go code from proto/, docs/ from the schema and examples/
```

| Path | |
|---|---|
| `internal/provider` | The provider: one resource implementation for every writable kind, data sources, diagnostics |
| `internal/client` | mTLS to the proxy, bot login and renewal, retries, error explanations |
| `internal/specjson` | `spec` between Terraform values and canonical JSON |
| `internal/fakelimen` | The in-memory Limen for tests and `make fake` |
| `proto/` | The management API contract, copied from Limen with `scripts/sync-proto.sh` |
| `docs/` | Generated registry docs: edit `templates/` and `examples/` |

Pull requests start as drafts; CI runs once they are marked ready. Commits are conventional (`feat:`, `fix:`, `docs:`). See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md).

## Releases

Nothing is released yet: the first release, 0.1.0, waits until the provider is verified against a control plane that serves the management API.

Merging the release-please pull request tags the version and drafts a GitHub release. CI then re-runs on that commit, builds and GPG-signs the provider, and attaches it in the layout the [Terraform Registry](https://registry.terraform.io/providers/limen-dev/limen) and [OpenTofu Registry](https://search.opentofu.org/provider/limen-dev/limen) read. Publishing the draft releases it. [RELEASING.md](RELEASING.md) has the one-time registry setup.

## Security

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md). The provider never logs tokens or keys, and never needs `limen:admin`.

## License

[Apache-2.0](LICENSE), the same as Limen.
