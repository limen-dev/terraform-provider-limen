package provider_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"google.golang.org/grpc/codes"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	"github.com/limen-dev/terraform-provider-limen/internal/fakelimen"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

const kindResource = managev1.DocumentKind_DOCUMENT_KIND_RESOURCE

// A person writes a full resource: create, no phantom diff for a default
// Limen leaves out, update, import, delete.
func TestAccResourceLifecycle(t *testing.T) {
	e := newEnv(t)
	cfg := func(desc, endpoint string) string {
		return e.adminProvider("") + fmt.Sprintf(`
resource "limen_resource" "web" {
  name        = "web-staging-1"
  description = %q
  labels      = { env = "staging" }
  spec = {
    ssh = {
      address         = %q
      host_key        = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
      port_forwarding = false # a default: Limen leaves it out, and the plan must stay empty
    }
    worker_selector = { pool = "staging" }
  }
}
`, desc, endpoint)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: cfg("first", "10.0.1.15:22"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("id"), knownvalue.StringExact("web-staging-1")),
					statecheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("channel"), knownvalue.StringExact("api")),
					statecheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("source"), knownvalue.StringExact("terraform/test")),
					statecheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("namespace"), knownvalue.StringExact("default")),
					statecheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("updated_by"), knownvalue.StringExact("alice")),
					statecheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("effective_labels"), knownvalue.MapExact(map[string]knownvalue.Check{
						"env": knownvalue.StringExact("staging"),
					})),
				},
				Check: func(*terraform.State) error {
					d := e.srv.Document(kindResource, "web-staging-1")
					if d == nil || d.SpecJson != `{"ssh":{"address":"10.0.1.15:22","host_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"},"worker_selector":{"pool":"staging"}}` {
						return fmt.Errorf("stored spec: %v", d)
					}
					return nil
				},
			},
			{
				Config: cfg("second", "10.0.1.16:22"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("limen_resource.web", plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue("limen_resource.web", tfjsonpath.New("etag")),
						plancheck.ExpectKnownValue("limen_resource.web", tfjsonpath.New("created_at"), knownvalue.NotNull()),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("limen_resource.web", "description", "second"),
					resource.TestCheckResourceAttr("limen_resource.web", "spec.ssh.address", "10.0.1.16:22"),
				),
			},
			{
				ResourceName:      "limen_resource.web",
				ImportState:       true,
				ImportStateId:     "resource/web-staging-1",
				ImportStateVerify: true,
				// Imported, the spec is Limen's spelling, without the default.
				ImportStateVerifyIgnore: []string{"spec"},
			},
		},
		CheckDestroy: func(*terraform.State) error {
			if e.srv.Document(kindResource, "web-staging-1") != nil {
				return fmt.Errorf("still in Limen after destroy")
			}
			return nil
		},
	})
}

// The motivating case: a team's bot completes a profile, in its own source,
// in the grant's namespace, with the profile's fixed parts never as drift.
func TestAccBotCompletesProfile(t *testing.T) {
	e := newEnv(t)
	e.asGitHubBot()
	cfg := func(endpoint string) string {
		return e.botProvider("") + fmt.Sprintf(`
resource "limen_resource" "orders_db" {
  name    = "payments-orders-db"
  profile = %q
  spec = {
    postgres = { endpoint = %q }
  }
  labels = { env = "staging" }
}

data "limen_caller" "me" {}

output "effective" { value = jsondecode(limen_resource.orders_db.effective_spec_json) }
`, profile, endpoint)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: cfg("orders.c9xk2p.eu-west-1.rds.amazonaws.com:5432"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("limen_resource.orders_db", tfjsonpath.New("namespace"), knownvalue.StringExact("payments")),
					statecheck.ExpectKnownValue("limen_resource.orders_db", tfjsonpath.New("source"), knownvalue.StringExact("terraform/payments-prod")),
					statecheck.ExpectKnownValue("limen_resource.orders_db", tfjsonpath.New("updated_by"), knownvalue.StringExact("bot:"+botName)),
					statecheck.ExpectKnownValue("limen_resource.orders_db", tfjsonpath.New("labels"), knownvalue.MapExact(map[string]knownvalue.Check{
						"env": knownvalue.StringExact("staging"),
					})),
					statecheck.ExpectKnownValue("limen_resource.orders_db", tfjsonpath.New("effective_labels"), knownvalue.MapExact(map[string]knownvalue.Check{
						"env":  knownvalue.StringExact("staging"),
						"team": knownvalue.StringExact("payments"),
					})),
					statecheck.ExpectKnownOutputValueAtPath("effective", tfjsonpath.New("postgres").AtMapKey("region"), knownvalue.StringExact("eu-west-1")),
					statecheck.ExpectKnownOutputValueAtPath("effective", tfjsonpath.New("worker_selector").AtMapKey("network"), knownvalue.StringExact("payments-vpc")),
					statecheck.ExpectKnownValue("data.limen_caller.me", tfjsonpath.New("principal"), knownvalue.StringExact("bot:"+botName)),
					statecheck.ExpectKnownValue("data.limen_caller.me", tfjsonpath.New("grant").AtMapKey("namespace"), knownvalue.StringExact("payments")),
					statecheck.ExpectKnownValue("data.limen_caller.me", tfjsonpath.New("grant").AtMapKey("profiles"), knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact(profile)})),
				},
			},
			{
				// Outside the profile's host bounds: refused at plan by the dry run.
				Config:      cfg("orders.someone-else.rds.amazonaws.com:5432"),
				ExpectError: regexp.MustCompile(`(?s)Outside this writer's grant.*outside\s+the\s+profile's\s+hosts`),
			},
		},
	})
	if e.minted.Load() == 0 {
		t.Fatal("the provider never asked GitHub for a token")
	}
}

func TestAccBotRefusals(t *testing.T) {
	e := newEnv(t)
	e.asGitHubBot()
	for name, c := range map[string]struct{ body, err string }{
		"name outside the patterns": {
			`resource "limen_resource" "x" {
  name    = "billing-db"
  profile = "payments-postgres"
  spec    = { postgres = { endpoint = "a.c9xk2p.eu-west-1.rds.amazonaws.com:5432" } }
}`, `(?s)Outside this writer's grant.*name patterns`,
		},
		"field the profile fixes": {
			`resource "limen_resource" "x" {
  name    = "payments-x"
  profile = "payments-postgres"
  spec    = { postgres = { endpoint = "a.c9xk2p.eu-west-1.rds.amazonaws.com:5432", iam_auth = true } }
}`, `(?s)Outside this writer's grant.*postgres.iam_auth is not settable`,
		},
		"label the profile fixes": {
			`resource "limen_resource" "x" {
  name    = "payments-x"
  profile = "payments-postgres"
  spec    = { postgres = { endpoint = "a.c9xk2p.eu-west-1.rds.amazonaws.com:5432" } }
  labels  = { team = "billing" }
}`, `(?s)label "team" is fixed`,
		},
		"a full resource": {
			`resource "limen_resource" "x" {
  name = "payments-x"
  spec = { tcp = { address = "10.0.0.1:5432" } }
}`, `writers create resources only by completing a profile`,
		},
		"a built-in role": {
			`resource "limen_role_mapping" "x" {
  name = "payments-admins"
  spec = [{ idp_group = "payments-eng", roles = ["limen:admin"] }]
}`, `(?s)never name a built-in role`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: factories,
				Steps:                    []resource.TestStep{{Config: e.botProvider("") + c.body, ExpectError: regexp.MustCompile(c.err)}},
			})
		})
	}
}

func TestAccWrongSpecShape(t *testing.T) {
	e := newEnv(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: e.adminProvider("") + `
resource "limen_role_mapping" "x" {
  name = "okta"
  spec = { idp_group = "a", roles = ["b"] }
}`,
			ExpectError: regexp.MustCompile(`A role mapping's spec is a list`),
		}},
	})
}

// A change made outside Terraform shows as drift and is put back; a change
// made between plan and apply is refused, never overwritten.
func TestAccDriftAndConcurrentChange(t *testing.T) {
	e := newEnv(t)
	cfg := e.adminProvider("") + `
resource "limen_resource" "db" {
  name = "orders-db"
  spec = { postgres = { endpoint = "orders.internal:5432", default_database = "orders" } }
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					if err := e.srv.EditDocument(kindResource, "orders-db", func(d *managev1.Document) {
						d.SpecJson = `{"postgres":{"default_database":"orders","endpoint":"evil.internal:5432"}}`
					}); err != nil {
						t.Fatal(err)
					}
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("limen_resource.db", plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if d := e.srv.Document(kindResource, "orders-db"); d.SpecJson != `{"postgres":{"default_database":"orders","endpoint":"orders.internal:5432"}}` {
						return fmt.Errorf("drift not repaired: %s", d.SpecJson)
					}
					return nil
				},
			},
		},
	})
}

// A document another channel owns is never written in place: the error names
// the owner. Only a person with limen:admin can take it over, and the plan
// says so.
func TestAccOwnedElsewhere(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.Seed(&managev1.Document{Kind: kindResource, Name: "legacy-db", SpecJson: `{"tcp":{"address":"10.0.0.9:5432"}}`}, nil); err != nil {
		t.Fatal(err)
	}
	cfg := func(owner string) string {
		return e.adminProvider("") + fmt.Sprintf(`
resource "limen_resource" "legacy" {
  name = "legacy-db"
  spec = { tcp = { address = "10.0.0.10:5432" } }
  %s
}`, owner)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      cfg(""),
				ExpectError: regexp.MustCompile(`(?s)Name already taken.*channel "git", source "default"`),
			},
			{
				Config:             cfg(""),
				ImportState:        true,
				ImportStateId:      "legacy-db",
				ResourceName:       "limen_resource.legacy",
				ImportStatePersist: true,
			},
			{
				// Kept with its owner: the write is refused, naming it.
				Config:      cfg(`source = "default"` + "\n" + `namespace = "default"`),
				ExpectError: regexp.MustCompile(`(?s)Document is managed somewhere else.*channel "git"`),
			},
			{
				// Moved to this configuration: an admin may.
				Config: cfg(""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("limen_resource.legacy", tfjsonpath.New("channel"), knownvalue.StringExact("api")),
					statecheck.ExpectKnownValue("limen_resource.legacy", tfjsonpath.New("source"), knownvalue.StringExact("terraform/test")),
				},
			},
		},
	})
}

// A widening change is held; apply names it, and after a person approves it
// the next apply finds the document in place.
func TestAccHeldChange(t *testing.T) {
	e := newEnv(t)
	e.asGitHubBot()
	e.srv.HoldWritesTo(managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING)
	cfg := e.botProvider(`change_reason = "payments engineers read staging"`) + `
resource "limen_role_mapping" "payments" {
  name = "payments-staging"
  spec = [{ idp_group = "payments-eng", roles = ["payments-read"] }]
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      cfg,
				ExpectError: regexp.MustCompile(`(?s)Change is waiting for approval.*change:\s+chg-0001.*limen policy\s+changes approve chg-0001`),
			},
			{
				// Applying again before anyone decides replaces the request.
				Config:      cfg,
				ExpectError: regexp.MustCompile(`(?s)change:\s+chg-0002.*replaces:\s+chg-0001`),
			},
			{
				PreConfig: func() {
					if got := e.srv.PendingChanges(); len(got) != 1 || got[0] != "chg-0002" {
						t.Fatalf("pending: %v", got)
					}
					if err := e.srv.Decide("chg-0002", true, "carol", "ok"); err != nil {
						t.Fatal(err)
					}
				},
				Config: cfg,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("limen_role_mapping.payments", tfjsonpath.New("updated_by"), knownvalue.StringExact("bot:"+botName)),
				},
			},
		},
	})
}

func TestAccHeldChangeWaits(t *testing.T) {
	e := newEnv(t)
	e.asGitHubBot()
	e.srv.HoldWritesTo(managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			for _, id := range e.srv.PendingChanges() {
				_ = e.srv.Decide(id, true, "carol", "approved while waiting")
			}
		}
	}()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: e.botProvider(`approval_timeout = "30s"`) + `
resource "limen_role_mapping" "payments" {
  name = "payments-staging"
  spec = [{ idp_group = "payments-eng", roles = ["payments-read"] }]
}`,
			Check: resource.TestCheckResourceAttrSet("limen_role_mapping.payments", "etag"),
		}},
	})
}

func TestAccRejectedChange(t *testing.T) {
	e := newEnv(t)
	e.asGitHubBot()
	e.srv.HoldWritesTo(managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING)
	go func() {
		for range 100 {
			time.Sleep(200 * time.Millisecond)
			for _, id := range e.srv.PendingChanges() {
				_ = e.srv.Decide(id, false, "carol", "too broad")
				return
			}
		}
	}()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: e.botProvider(`approval_timeout = "30s"`) + `
resource "limen_role_mapping" "payments" {
  name = "payments-staging"
  spec = [{ idp_group = "payments-eng", roles = ["payments-read"] }]
}`,
			ExpectError: regexp.MustCompile(`(?s)Change was not applied.*was\s+rejected\s+by\s+carol:\s+too\s+broad`),
		}},
	})
}

// Moving a resource to another state (and so another source) transfers it.
func TestAccTransferBetweenSources(t *testing.T) {
	e := newEnv(t)
	body := `
resource "limen_policy_test" "t" {
  name = "prod-is-locked-down"
  spec = {
    cases = [{
      name      = "developers cannot ssh to prod as root"
      who       = { groups = ["developers"] }
      resource  = "web-prod"
      principal = { login = "root" }
      expect    = "deny"
    }]
  }
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: e.adminProvider("") + body},
			{
				Config: e.adminProvider("") + `
resource "limen_policy_test" "t" {
  name   = "prod-is-locked-down"
  source = "terraform/security"
  spec = {
    cases = [{
      name      = "developers cannot ssh to prod as root"
      who       = { groups = ["developers"] }
      resource  = "web-prod"
      principal = { login = "root" }
      expect    = "deny"
    }]
  }
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("limen_policy_test.t", plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					d := e.srv.Document(managev1.DocumentKind_DOCUMENT_KIND_POLICY_TEST, "prod-is-locked-down")
					if d.GetOwnership().GetSource() != "terraform/security" {
						return fmt.Errorf("not transferred: %v", d.GetOwnership())
					}
					return nil
				},
			},
		},
	})
}

// Writes survive Limen being briefly unable to audit, with the same request id.
func TestAccRetriesWrites(t *testing.T) {
	e := newEnv(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			PreConfig: func() {
				// The dry run during plan, then the write itself.
				e.srv.FailNext("CreateDocument", 3, codes.Unavailable, client.ReasonAuditUnavailable, 0)
			},
			Config: e.adminProvider("") + `
resource "limen_role" "r" {
  name = "dev-readonly"
  spec = { cert_ttl = "10m", allow = [{ resources = { kind = ["postgres"], labels = { env = ["staging"] } }, db = { roles = ["limen_read"] } }] }
}`,
			Check: resource.TestCheckResourceAttr("limen_role.r", "spec.cert_ttl", "10m"),
		}},
	})
}

func TestAccOffline(t *testing.T) {
	e := newEnv(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				// No credentials are configured: offline never logs in.
				Config: fmt.Sprintf(`
provider "limen" {
  address = %q
  offline = true
  default_source = "terraform/pr"
}
resource "limen_resource" "x" {
  name = "payments-x"
  spec = { tcp = { address = "10.0.0.1:5432" } }
}`, e.srv.Address),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: fmt.Sprintf(`
provider "limen" {
  address = %q
  offline = true
  default_source = "terraform/pr"
}
resource "limen_resource" "x" {
  name = "payments-x"
  spec = { tcp = { address = "10.0.0.1:5432" } }
}`, e.srv.Address),
				ExpectError: regexp.MustCompile(`Limen provider is offline`),
			},
		},
	})
	for _, call := range e.srv.Calls() {
		t.Fatalf("offline called Limen: %s", call)
	}
}

func TestAccNoSource(t *testing.T) {
	e := newEnv(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "limen" {
  address        = %q
  ca_certificate = %s
  client_certificate = {
    cert_pem = %s
    key_pem  = %s
  }
}
resource "limen_role" "r" {
  name = "r"
  spec = { allow = [] }
}`, e.srv.Address, heredoc(e.srv.CAPEM), heredoc(e.adminCert), heredoc(e.adminKey)),
			ExpectError: regexp.MustCompile(`No source for the new document`),
		}},
	})
}

func TestAccDataSources(t *testing.T) {
	e := newEnv(t)
	e.asGitHubBot()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: e.botProvider("") + `
resource "limen_resource" "a" {
  name    = "payments-a"
  profile = "payments-postgres"
  spec    = { postgres = { endpoint = "a.c9xk2p.eu-west-1.rds.amazonaws.com:5432" } }
  labels  = { env = "dev" }
}
resource "limen_resource" "b" {
  name    = "payments-b"
  profile = "payments-postgres"
  spec    = { postgres = { endpoint = "b.c9xk2p.eu-west-1.rds.amazonaws.com:5432" } }
  labels  = { env = "staging" }
}

data "limen_resource_profile" "p" {
  name = "payments-postgres"
}

data "limen_documents" "staging" {
  kind       = "resource"
  labels     = { env = "staging", team = "payments" }
  depends_on = [limen_resource.a, limen_resource.b]
}

data "limen_resource" "a" {
  name = limen_resource.a.name
}

data "limen_schema" "role" {
  kind = "role"
}

output "settable" { value = keys(data.limen_resource_profile.p.spec.settable) }
`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownOutputValue("settable", knownvalue.SetExact([]knownvalue.Check{
					knownvalue.StringExact("postgres.default_database"), knownvalue.StringExact("postgres.endpoint"),
				})),
				statecheck.ExpectKnownValue("data.limen_documents.staging", tfjsonpath.New("names"), knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("payments-b")})),
				statecheck.ExpectKnownValue("data.limen_resource.a", tfjsonpath.New("spec").AtMapKey("postgres").AtMapKey("endpoint"),
					knownvalue.StringExact("a.c9xk2p.eu-west-1.rds.amazonaws.com:5432")),
				statecheck.ExpectKnownValue("data.limen_resource.a", tfjsonpath.New("effective_spec").AtMapKey("postgres").AtMapKey("iam_auth"), knownvalue.Bool(true)),
				statecheck.ExpectKnownValue("data.limen_resource.a", tfjsonpath.New("channel"), knownvalue.StringExact("api")),
				statecheck.ExpectKnownValue("data.limen_schema.role", tfjsonpath.New("json_schema"), jsonObject{}),
			},
		}},
	})
}

// jsonObject checks that a string holds a JSON object.
type jsonObject struct{}

func (jsonObject) CheckValue(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("not a string: %T", v)
	}
	var m map[string]any
	return json.Unmarshal([]byte(s), &m)
}

func (jsonObject) String() string { return "a JSON object" }

// Renaming replaces the document: the plan dry-runs a create of the new name.
func TestAccRename(t *testing.T) {
	e := newEnv(t)
	cfg := func(name, addr string) string {
		return e.adminProvider("") + fmt.Sprintf(`
resource "limen_resource" "db" {
  name = %q
  spec = { tcp = { address = %q } }
}`, name, addr)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: cfg("orders-db", "10.0.0.1:5432")},
			{
				Config: cfg("orders-db-2", "10.0.0.2:5432"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("limen_resource.db", plancheck.ResourceActionReplace)},
				},
				Check: func(*terraform.State) error {
					if e.srv.Document(kindResource, "orders-db") != nil || e.srv.Document(kindResource, "orders-db-2") == nil {
						return fmt.Errorf("not replaced")
					}
					return nil
				},
			},
		},
	})
}

// Limen's Phase 14 stage 1 writes resources only. A resource's whole
// lifecycle works against it; the other kinds say why they can't yet.
func TestAccStage1(t *testing.T) {
	e := newEnvWith(t, fakelimen.Options{Stage1: true})
	e.asGitHubBot()
	cfg := func(db, labels string) string {
		return e.botProvider("") + fmt.Sprintf(`
resource "limen_resource" "orders_db" {
  name    = "payments-orders-db"
  profile = %q
  spec    = { postgres = { endpoint = %q } }
  labels  = %s
}`, profile, db, labels)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: cfg("orders.c9xk2p.eu-west-1.rds.amazonaws.com:5432", `{ env = "dev" }`)},
			{
				// An update and a delete both name the owner; Limen refuses them otherwise.
				Config: cfg("orders2.c9xk2p.eu-west-1.rds.amazonaws.com:5432", `{ env = "staging" }`),
				Check:  resource.TestCheckResourceAttr("limen_resource.orders_db", "labels.env", "staging"),
			},
			{
				Config: e.botProvider("") + `
resource "limen_role" "r" {
  name = "payments-read"
  spec = { allow = [] }
}`,
				ExpectError: regexp.MustCompile(`(?s)This Limen doesn't support that yet.*stage 1 writes\s+resources\s+only`),
			},
			{
				Config: e.botProvider("") + `
data "limen_schema" "role" {
  kind = "role"
}`,
				ExpectError: regexp.MustCompile(`This Limen doesn't support that yet`),
			},
		},
	})
}

// Terraform configures the provider more than once in a single apply, and a
// workload token works only once: the provider must log in once and reuse the
// certificate, or the second configuration is refused as a replay.
func TestAccOneLoginPerToken(t *testing.T) {
	e := newEnv(t)
	e.srv.AddBotToken("one-use-token", client.MethodOIDC, botName)
	t.Setenv("LIMEN_TEST_TOKEN", "one-use-token")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "limen" {
  address            = %q
  ca_certificate     = %s
  default_source     = "terraform/payments-prod"
  bot                = { method = "oidc", name = %q, token_env = "LIMEN_TEST_TOKEN" }
}
resource "limen_resource" "db" {
  name    = "payments-db"
  profile = %q
  spec    = { postgres = { endpoint = "a.c9xk2p.eu-west-1.rds.amazonaws.com:5432" } }
}`, e.srv.Address, heredoc(e.srv.CAPEM), botName, profile),
		}},
	})
	logins := 0
	for _, c := range e.srv.Calls() {
		if c == "BotLogin" {
			logins++
		}
	}
	if logins != 1 {
		t.Fatalf("BotLogin was called %d times; a one-use token allows exactly one", logins)
	}
}
