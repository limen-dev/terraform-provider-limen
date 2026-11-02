package provider_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// TestLiveAdminLifecycle runs against a real Limen control plane, not
// fakelimen, so that what the fake models is checked against the server:
//
//	LIMEN_LIVE=1 LIMEN_ADDRESS=localhost:38443 LIMEN_CA_FILE=... LIMEN_HOME=<an admin's limen login> \
//	  TF_ACC=1 go test ./internal/provider -run TestLive -count=1 -v
//
// It needs a person with limen:admin (the people-only kinds are refused to
// bots) and a bot-free corner of the policy: every document is named
// tfacc-<time> and removed again. See CONTRIBUTING.md.
func TestLiveAdminLifecycle(t *testing.T) {
	if os.Getenv("LIMEN_LIVE") == "" {
		t.Skip("set LIMEN_LIVE=1, LIMEN_ADDRESS, LIMEN_CA_FILE and LIMEN_HOME to run against a real Limen")
	}
	p := fmt.Sprintf("tfacc-%d", time.Now().Unix())
	cfg := func(maxDocs int) string {
		return fmt.Sprintf(`
provider "limen" {
  default_source = "terraform/%[1]s"
}

# Dependent documents in one apply: a dry run can't see the role before it exists.
resource "limen_role" "ci" {
  name = "%[1]s-ci"
  spec = {
    cert_ttl = "15m"
    allow = [{
      resources = { kind = ["ssh"], labels = { team = ["%[1]s"] } }
      ssh       = { logins = ["ubuntu"] }
    }]
  }
}

resource "limen_bot" "ci" {
  name = "%[1]s-bot"
  spec = {
    join = {
      method = "github_actions"
      issuer = "http://fake-ci-issuer:8130"
      match  = { repository = "acme/%[1]s", ref = "refs/heads/main", environment = "staging" }
    }
    roles   = [limen_role.ci.name]
    max_ttl = "1h"
  }
}

resource "limen_resource_profile" "ssh" {
  name = "%[1]s-ssh"
  spec = {
    template = { ssh = {}, worker_selector = { pool = "dev" } }
    settable = { "ssh.address" = { hosts = ["sshd-target"], ports = [22] }, "ssh.host_key" = {} }
    labels   = { fixed = { team = "%[1]s" }, allowed = { env = ["staging"] } }
  }
}

resource "limen_policy_writer" "ci" {
  name = "%[1]s-bot"
  spec = {
    bot           = limen_bot.ci.name
    namespace     = "%[1]s"
    names         = ["%[1]s-*"]
    kinds         = ["resource"]
    profiles      = [limen_resource_profile.ssh.name]
    max_documents = %[2]d
  }
}

resource "limen_resource" "host" {
  name   = "%[1]s-host"
  labels = { team = "%[1]s", env = "staging" }
  spec = {
    tcp             = { address = "sshd-target:22" }
    worker_selector = { pool = "dev" }
  }
}

resource "limen_role_mapping" "m" {
  name = "%[1]s-mapping"
  spec = [{ idp_group = "%[1]s-eng", roles = [limen_role.ci.name] }]
}

resource "limen_policy_test" "t" {
  name = "%[1]s-test"
  spec = {
    cases = [{
      name      = "the group reaches the host"
      who       = { groups = ["%[1]s-eng"] }
      resource  = limen_resource.host.name
      principal = { tcp = true }
      expect    = "allow"
    }]
  }
}
`, p, maxDocs)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				// resource.Test fails when the plan after apply isn't empty:
				// that is the check for phantom diffs against Limen's normalization.
				Config: cfg(5),
				Check:  resource.TestCheckResourceAttr("limen_policy_writer.ci", "spec.max_documents", "5"),
			},
			{
				Config: cfg(7),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("limen_policy_writer.ci", plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("limen_bot.ci", plancheck.ResourceActionNoop),
				}},
			},
			{
				ResourceName:            "limen_bot.ci",
				ImportState:             true,
				ImportStateId:           "bot/" + p + "-bot",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"spec"},
			},
		},
	})
}
