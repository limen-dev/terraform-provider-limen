---
page_title: "Limen Provider"
description: |-
  Manage Limen policy documents (resources, roles, role mappings, policy tests) through Limen's management API.
---

# Limen Provider

The Limen provider manages [Limen](https://github.com/limen-dev/limen) policy documents through its management API, `limen.manage.v1` ([ADR-0042](https://github.com/limen-dev/limen/blob/main/docs/adr/0042-management-api.md)). It works with Terraform and OpenTofu.

The case it is built for: a team's module creates a database and registers it in Limen **in the same plan, in its own state**, while the security or platform team manages roles, mappings, and bots from theirs.

```terraform
terraform {
  required_providers {
    limen = {
      source  = "limen-dev/limen"
      version = "~> 0.1"
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
```

## How it fits Limen's model

- **One owner per document.** Every document has an owner, `{channel, source, namespace}`. Documents this provider creates are owned by channel `api`, the provider's `default_source`, and its namespace. Another owner's document (from `limen admin apply`, the console, or another state's source) is never written: the plan fails and names the owner.
- **No lost updates.** Every write sends the document's etag. A change made in Limen between plan and apply is refused, never merged or overwritten. Changes made outside Terraform show as drift and are put back by the next apply.
- **Bots get the narrowest right, never `limen:admin`.** A bot writes through a `policy_writer` grant: one namespace, name patterns, kinds, and the resource profiles it may complete. `bot`, `discovery`, `resource_profile`, and `policy_writer` documents are written only by people, so they are data sources here, not resources.
- **Plans are real dry runs.** During plan the provider sends each change with `validate_only` (`plan_checks`): authorization, validation, profiles, and tests all run, and who gains or loses access is shown as a warning beside the diff.
- **Widening needs a person.** A bot's change that widens standing access is held for approval. The plan says so; apply fails naming the change, or waits for it with `approval_timeout`. After approval, apply again: the provider finds the document in place.

## Authentication

The provider picks the first of these that is configured:

1. **A bot** (`bot` block, or `LIMEN_BOT_METHOD`): logs in with the CI platform's workload identity token, so no secret is stored anywhere. Limen accepts each token once and issues a certificate of at most an hour.
   - `github_actions`: asks GitHub for a token. The job needs `permissions: id-token: write`.
   - `gitlab`: reads `$LIMEN_ID_TOKEN` (declare it under `id_tokens:` with `aud: limen`), or `token_env` / `token_file`.
   - `oidc`: any other issuer the bot's policy names, such as Buildkite, CircleCI, or a Kubernetes projected service account token (`token_file`, which is read again when the provider has to log in again).
2. **A client certificate** (`client_certificate`) you already have.
3. **The Limen CLI's login** from `limen login`, for running Terraform from a laptop. The address and CA default to the CLI's too.

The proxy's CA comes from `ca_certificate`, `ca_file`, `LIMEN_CA_CERTIFICATE`, `LIMEN_CA_FILE`, or the CA `limen login --ca-file` pinned.

### One token, one login

A bot's workload token works once. The provider logs in once per run and reuses the certificate when Terraform configures it again, as `terraform apply` does. A plan and an apply in separate runs each need their own token: GitHub Actions mints one per request, but a token in a variable or file works for one run only.

### Long applies

Bot certificates last at most an hour and are never refreshed. When the token source can mint another token (GitHub Actions, or a `token_file`), the provider logs in again two minutes before expiry. With a single token (`token`, `token_env`, Terraform Cloud's per-run token), an apply that outlasts the certificate fails its later writes; split it.

## Pull requests: plan offline

Limen never lets a pull request run log in as a bot (ADR-0019), so set `offline = true` (or `LIMEN_OFFLINE=true`) for pull request plans. Offline, the provider doesn't contact Limen: state isn't refreshed, nothing is dry-run, data sources can't be read, and apply is refused. The run on the protected branch does the online plan and the apply. See the [GitHub Actions guide](guides/github-actions.md).

## Compatibility

| Provider | Limen control plane | Terraform | OpenTofu |
|---|---|---|---|
| 0.x | Phase 14 stage 1 serves `limen.manage.v1` for resources; stage 2 adds roles, role mappings, policy tests, and `limen_schema` | 1.5 and later | 1.6 and later |

Against a control plane without the API, every call fails with "This Limen doesn't serve the management API". Against stage 1, writing a role, role mapping, or policy test fails with "This Limen doesn't support that yet": apply those documents from files until stage 2.

Reading a past `policy_version` in a data source is not served yet either.

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `address` (String) A Limen proxy's `host:port`, such as `limen.example.com:443`. Defaults to `$LIMEN_ADDRESS`, else the proxy the Limen CLI logged in to last.
- `approval_timeout` (String) How long apply waits for a person to approve a change Limen held because it widens standing access, such as `30m`. Default `0s`: fail at once, naming the change; after it is approved, apply again. Defaults to `$LIMEN_APPROVAL_TIMEOUT`.
- `bot` (Attributes) Log in as a Limen bot with the job's workload identity token. Without this block, `$LIMEN_BOT_METHOD` (and `$LIMEN_BOT_NAME`, `$LIMEN_BOT_AUDIENCE`, `$LIMEN_BOT_TOKEN_FILE`) configure it. (see [below for nested schema](#nestedatt--bot))
- `ca_certificate` (String) PEM of the CA that signs the proxy's certificate. Defaults to `$LIMEN_CA_CERTIFICATE`, then the file named by `ca_file`, then the CA the Limen CLI pinned for this proxy.
- `ca_file` (String) Path to a PEM file with the proxy's CA. Defaults to `$LIMEN_CA_FILE`.
- `change_reason` (String) Why, in a sentence, shown to approvers of held changes. Defaults to `$LIMEN_CHANGE_REASON`.
- `client_certificate` (Attributes) Use a Limen client certificate the caller already has, instead of logging in. (see [below for nested schema](#nestedatt--client_certificate))
- `default_namespace` (String) The namespace documents this configuration creates are owned by. Defaults to `$LIMEN_NAMESPACE`, else the bot's grant's namespace, else `default`.
- `default_source` (String) The source documents this configuration creates are owned by, such as `terraform/payments-prod`; one per Terraform state is the rule of thumb. Required to create documents. Defaults to `$LIMEN_SOURCE`. A source keeps writers that share a namespace from overwriting each other by accident; it is not a security boundary.
- `offline` (Boolean) Plan without contacting Limen. Pull request runs can never log in as a bot (ADR-0019), so plan them offline: state isn't refreshed, nothing is dry-run, and apply is refused. Defaults to `$LIMEN_OFFLINE`.
- `plan_checks` (Boolean) During plan, dry-run every change against Limen (`validate_only`): authorization, validation, profiles, tests, and who gains or loses access, shown as warnings. Default `true` (`$LIMEN_PLAN_CHECKS`).
- `source_ref` (String) Where changes come from, recorded in Limen's audit trail, such as a commit or a run URL. Defaults to `$LIMEN_SOURCE_REF`, else the GitHub Actions run or GitLab job URL when running there.
- `tls_server_name` (String) The name to verify the proxy's certificate against, when it differs from the address's host.

<a id="nestedatt--bot"></a>
### Nested Schema for `bot`

Required:

- `method` (String) The bot's join method: `github_actions`, `gitlab`, or `oidc`.

Optional:

- `audience` (String) The audience to ask GitHub for. Defaults to the control plane's bot audience.
- `name` (String) The bot's name, needed only when the token could match more than one bot.
- `token` (String, Sensitive) The token itself. Prefer `token_env` or `token_file`, so the token isn't in configuration.
- `token_env` (String) The environment variable holding the token. Default `LIMEN_ID_TOKEN` for `gitlab` and `oidc`; `github_actions` asks GitHub for a token unless a token is named.
- `token_file` (String) A file holding the token, read again when the provider has to log in again (Kubernetes projected tokens).


<a id="nestedatt--client_certificate"></a>
### Nested Schema for `client_certificate`

Required:

- `cert_pem` (String) The certificate, PEM.
- `key_pem` (String, Sensitive) Its private key, PEM.
