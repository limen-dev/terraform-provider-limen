---
page_title: "Running in GitHub Actions"
subcategory: ""
description: |-
  Plan pull requests offline and apply from the protected branch as a Limen bot, with no stored secret.
---

# Running in GitHub Actions

A Limen bot logs in with the job's GitHub OIDC token. Nothing is stored in a secret, and pull request runs can never log in as a bot, so they plan offline.

## 1. The bot and its grant (written by a person, in your policy repository)

```yaml
kind: bot
version: v1
metadata: { name: payments-terraform }
spec:
  join:
    method: github_actions
    match: { repository: acme/payments-infra, ref: refs/heads/main, environment: production }
  roles: []
---
kind: policy_writer
version: v1
metadata: { name: payments-terraform }
spec:
  bot: payments-terraform
  namespace: payments
  names: ["payments-*"]
  kinds: [resource, policy_test]
  profiles: [payments-postgres]
  approvers: { groups: [security] }
```

## 2. The provider

```terraform
provider "limen" {
  address        = "limen.example.com:443"
  ca_file        = "${path.module}/limen-ca.pem"
  default_source = "terraform/payments-prod"
  bot            = { method = "github_actions", name = "payments-terraform" }
}
```

`LIMEN_OFFLINE` switches it to offline mode without changing the configuration.

## 3. The workflow

```yaml
name: terraform
on:
  pull_request:
  push:
    branches: [main]

jobs:
  plan:
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    env:
      LIMEN_OFFLINE: "true"   # pull request runs never log in as a bot
    steps:
      - uses: actions/checkout@v5
      - uses: hashicorp/setup-terraform@v3
      - run: terraform init -input=false
      - run: terraform plan -input=false -refresh=false

  apply:
    if: github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    environment: production     # the bot's match names it
    permissions:
      contents: read
      id-token: write           # lets the provider ask GitHub for a token
    env:
      LIMEN_APPROVAL_TIMEOUT: 30m   # wait for a person to approve widening changes
    steps:
      - uses: actions/checkout@v5
      - uses: hashicorp/setup-terraform@v3
      - run: terraform init -input=false
      - run: terraform plan -input=false -out=tfplan
      - run: terraform apply -input=false tfplan
```

The provider records the run's URL as the change's `source_ref` in Limen's audit trail. Limen also records the run's CI facts (repository, workflow, commit, and who started it) from the token.

With OpenTofu, use `opentofu/setup-opentofu` and `tofu` in place of `terraform`.
