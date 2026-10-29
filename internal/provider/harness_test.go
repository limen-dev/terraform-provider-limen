package provider_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	"github.com/limen-dev/terraform-provider-limen/internal/fakelimen"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
	"github.com/limen-dev/terraform-provider-limen/internal/provider"
)

var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"limen": providerserver.NewProtocol6WithError(provider.New("test")()),
}

// env is a fake Limen with the documents and bots the tests share.
type env struct {
	t   *testing.T
	srv *fakelimen.Server
	// adminCert and adminKey are a person's credentials with limen:admin.
	adminCert, adminKey string
	// minted counts tokens the fake GitHub token service handed out.
	minted atomic.Int64
}

const (
	botName = "payments-terraform"
	profile = "payments-postgres"
)

func newEnv(t *testing.T) *env {
	t.Helper()
	srv, err := fakelimen.Start(fakelimen.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.AddAdmin("alice")
	cert, key, err := srv.IssueCertificate("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv.AddWriter(botName, fakelimen.Writer{
		Name:         botName,
		Namespace:    "payments",
		NamePatterns: []string{"payments-*"},
		Kinds: []managev1.DocumentKind{
			managev1.DocumentKind_DOCUMENT_KIND_RESOURCE,
			managev1.DocumentKind_DOCUMENT_KIND_ROLE,
			managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING,
			managev1.DocumentKind_DOCUMENT_KIND_POLICY_TEST,
		},
		Profiles:       []string{profile},
		MaxDocuments:   200,
		ApproverGroups: []string{"security"},
	})
	if err := srv.Seed(&managev1.Document{
		Kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE,
		Name: profile,
		SpecJson: `{
			"template": {"postgres": {"iam_auth": true, "region": "eu-west-1", "tls_ca_file": "/etc/limen/rds-ca.pem"},
			             "worker_selector": {"network": "payments-vpc"}},
			"settable": {"postgres.endpoint": {"hosts": ["*.c9xk2p.eu-west-1.rds.amazonaws.com"], "ports": [5432]},
			             "postgres.default_database": {}},
			"labels": {"fixed": {"team": "payments"}, "allowed": {"env": ["dev", "staging"]}}
		}`,
	}, nil); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, srv: srv, adminCert: cert, adminKey: key}
}

// asGitHubBot points the provider at a fake GitHub Actions token service
// that mints a fresh one-use token for the bot on every request, as GitHub
// does. Tests that use it can't run in parallel (they set the environment).
func (e *env) asGitHubBot() {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runner-token" || r.URL.Query().Get("audience") != "limen" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		tok := fmt.Sprintf("gh-%d", e.minted.Add(1))
		e.srv.AddBotToken(tok, client.MethodGitHubActions, botName)
		fmt.Fprintf(w, `{"value":%q}`, tok)
	}))
	e.t.Cleanup(gh.Close)
	e.t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", gh.URL+"/token")
	e.t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-token")
}

func heredoc(s string) string {
	return "<<-EOT\n" + strings.TrimSpace(s) + "\nEOT"
}

// adminProvider is a provider block logged in as alice.
func (e *env) adminProvider(extra string) string {
	return fmt.Sprintf(`
provider "limen" {
  address        = %q
  ca_certificate = %s
  default_source = "terraform/test"
  client_certificate = {
    cert_pem = %s
    key_pem  = %s
  }
  %s
}
`, e.srv.Address, heredoc(e.srv.CAPEM), heredoc(e.adminCert), heredoc(e.adminKey), extra)
}

// botProvider is a provider block logged in as the payments bot through
// GitHub Actions. Call asGitHubBot first.
func (e *env) botProvider(extra string) string {
	return fmt.Sprintf(`
provider "limen" {
  address        = %q
  ca_certificate = %s
  default_source = "terraform/payments-prod"
  bot = {
    method = "github_actions"
    name   = %q
  }
  %s
}
`, e.srv.Address, heredoc(e.srv.CAPEM), botName, extra)
}
