---
page_title: "Who writes what: platform and team states"
subcategory: ""
description: |-
  Keep bots, profiles, and writer grants in a state a person applies, and let each team's pipeline complete a profile as a bot.
---

# Who writes what: platform and team states

Limen lets only **people** write `bot`, `discovery`, `resource_profile`, and `policy_writer` documents. A pipeline's bot can't widen its own grant, and Limen refuses its login for those kinds (`OUTSIDE_GRANT`, rule `kind`). So split the work into two kinds of Terraform state:

| State | Applied by | Logs in as | Manages |
|---|---|---|---|
| **Platform** (one) | A person, or a pipeline using a person's client certificate | `limen login` (a person with `limen:admin`) | `limen_role`, `limen_bot`, `limen_resource_profile`, `limen_policy_writer`, `limen_role_mapping` |
| **Team** (one per team) | The team's CI | The team's bot | `limen_resource` that completes a profile, and what the grant allows |

## The platform state

```terraform
provider "limen" {
  address        = "limen.example.com:443"
  default_source = "terraform/platform" # one source per state
  # no `bot` block: the provider reuses `limen login`
}

resource "limen_role" "payments_ci" {
  name = "payments-ci"
  spec = { cert_ttl = "15m", allow = [{ resources = { kind = ["postgres"], labels = { team = ["payments"] } }, db = { roles = ["limen_read"] } }] }
}

resource "limen_bot" "payments_terraform" {
  name = "payments-terraform"
  spec = {
    join    = { method = "github_actions", match = { repository = "acme/payments-infra", ref = "refs/heads/main", environment = "prod" } }
    roles   = [limen_role.payments_ci.name]
    max_ttl = "15m"
  }
}

resource "limen_resource_profile" "payments_postgres" {
  name = "payments-postgres"
  spec = {
    template = { postgres = { iam_auth = true, region = "eu-west-1" }, worker_selector = { network = "payments-vpc" } }
    settable = { "postgres.endpoint" = { hosts = ["*.c9xk2p.eu-west-1.rds.amazonaws.com"], ports = [5432] } }
    labels   = { fixed = { team = "payments" }, allowed = { env = ["dev", "staging"] } }
  }
}

resource "limen_policy_writer" "payments_terraform" {
  name = "payments-terraform"
  spec = {
    bot       = limen_bot.payments_terraform.name
    namespace = "payments"
    names     = ["payments-*"]
    kinds     = ["resource"]
    profiles  = [limen_resource_profile.payments_postgres.name]
  }
}
```

Terraform orders the writes by reference. A plan's dry run sees only the policy **as it is now**, so a bot that names a role created in the same apply is checked again at apply and shown as a warning, not an error, at plan time.

Removing is ordered the same way, in reverse. Limen refuses to delete a bot or profile that a grant still names, or a profile that resources still complete (`IN_USE`), so delete the team's resources first.

## The team state

See [Running in GitHub Actions](github-actions.md). It uses `limen_resource` with `profile = "payments-postgres"`.

A change that gives someone **standing** access is held for a person. That includes anything that comes from the bot's own roles and mappings, so a resource that one of them selects waits too. Plans warn about it (the dry run sets `fail_if_approval_needed`). Apply prints the change id and stops, or waits for the decision with `approval_timeout`. After approval, apply again: the provider finds the document in place and the plan is empty.

```console
$ limen manage changes show 01a197a8-a2d2-77b3-bc0b-62de36c7e155
$ limen manage changes approve 01a197a8-a2d2-77b3-bc0b-62de36c7e155 --note "reviewed"
```
