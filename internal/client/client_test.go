package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	"github.com/limen-dev/terraform-provider-limen/internal/fakelimen"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

func start(t *testing.T, opts fakelimen.Options) *fakelimen.Server {
	t.Helper()
	srv, err := fakelimen.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.AddWriter("payments-terraform", fakelimen.Writer{
		Name: "payments-terraform", Namespace: "payments", NamePatterns: []string{"payments-*"},
		Kinds: []managev1.DocumentKind{managev1.DocumentKind_DOCUMENT_KIND_RESOURCE}, Profiles: []string{"payments-postgres"},
	})
	return srv
}

func noSleep(context.Context, time.Duration) error { return nil }

func connectBot(t *testing.T, srv *fakelimen.Server, token string) *client.Client {
	t.Helper()
	srv.AddBotToken(token, client.MethodOIDC, "payments-terraform")
	c, err := client.New(context.Background(), client.Config{
		Address:   srv.Address,
		CACertPEM: srv.CAPEM,
		Auth:      &client.BotAuth{Method: client.MethodOIDC, Token: token},
		Retry:     client.RetryPolicy{Sleep: noSleep},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestBotLoginAndDescribeCaller(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	c := connectBot(t, srv, "tok-1")
	if c.Principal() != "bot:payments-terraform" {
		t.Fatalf("principal %q", c.Principal())
	}
	resp, err := c.Manage.DescribeCaller(context.Background(), &managev1.DescribeCallerRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetGrant().GetNamespace() != "payments" {
		t.Fatalf("grant %v", resp.GetGrant())
	}
	if c.ServerVersion() != "0.14.0-fake" {
		t.Fatalf("server version %q", c.ServerVersion())
	}
}

func TestTokensWorkOnce(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	connectBot(t, srv, "tok-once")
	_, err := client.New(context.Background(), client.Config{
		Address: srv.Address, CACertPEM: srv.CAPEM,
		Auth: &client.BotAuth{Method: client.MethodOIDC, Token: "tok-once"},
	})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("a token works once, got %v", err)
	}
}

func TestUntrustedProxyIsExplained(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	other := start(t, fakelimen.Options{})
	_, err := client.New(context.Background(), client.Config{
		Address: srv.Address, CACertPEM: other.CAPEM,
		Auth:  &client.BotAuth{Method: client.MethodOIDC, Token: "x"},
		Retry: client.RetryPolicy{Sleep: noSleep},
	})
	if err == nil {
		t.Fatal("a proxy signed by another CA must be refused")
	}
	summary, detail := client.Explain(err)
	if summary != "Can't reach Limen" || !strings.Contains(detail, "ca_certificate") {
		t.Fatalf("got %q / %q", summary, detail)
	}
}

func TestStructuredErrors(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	c := connectBot(t, srv, "tok-2")
	_, err := c.Manage.CreateDocument(context.Background(), &managev1.CreateDocumentRequest{
		Document: &managev1.Document{
			Kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE, Name: "billing-db",
			SpecJson: `{}`, Profile: "payments-postgres",
		},
		Ownership: &managev1.Ownership{Source: "terraform/test", Namespace: "payments"},
	})
	if !client.Is(err, client.ReasonOutsideGrant) {
		t.Fatalf("want OUTSIDE_GRANT, got %v", err)
	}
	summary, detail := client.Explain(err)
	if summary != "Outside this writer's grant" || !strings.Contains(detail, "name patterns") {
		t.Fatalf("got %q / %q", summary, detail)
	}
}

func TestRetriesKeepTheRequest(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	c := connectBot(t, srv, "tok-3")
	srv.FailNext("DescribeCaller", 2, codes.Unavailable, client.ReasonAuditUnavailable, 0)
	srv.FailNext("GetDocument", 1, codes.ResourceExhausted, client.ReasonRateLimited, time.Second)
	if _, err := c.Manage.DescribeCaller(context.Background(), &managev1.DescribeCallerRequest{}); err != nil {
		t.Fatalf("UNAVAILABLE is retried: %v", err)
	}
	if _, err := c.GetDocument(context.Background(), managev1.DocumentKind_DOCUMENT_KIND_RESOURCE, "payments-x"); err != nil {
		t.Fatalf("RATE_LIMITED is retried: %v", err)
	}
	srv.FailNext("DescribeCaller", 1, codes.FailedPrecondition, client.ReasonEtagMismatch, 0)
	if _, err := c.Manage.DescribeCaller(context.Background(), &managev1.DescribeCallerRequest{}); !client.Is(err, client.ReasonEtagMismatch) {
		t.Fatalf("FAILED_PRECONDITION isn't retried: %v", err)
	}
	calls := strings.Join(srv.Calls(), ",")
	if strings.Count(calls, "DescribeCaller") != 4 {
		t.Fatalf("calls: %s", calls)
	}
}

func TestRetryableBounds(t *testing.T) {
	p := client.RetryPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second}
	limited := &client.APIError{Code: codes.ResourceExhausted, Reason: client.ReasonRateLimited, RetryAfter: time.Minute}
	if d, ok := client.Retryable(limited, 1, p); !ok || d != 4*time.Second {
		t.Fatalf("RetryInfo is capped by MaxDelay: %v %v", d, ok)
	}
	quota := &client.APIError{Code: codes.ResourceExhausted, Reason: client.ReasonQuotaExceeded}
	if _, ok := client.Retryable(quota, 1, p); ok {
		t.Fatal("a quota doesn't go away by waiting")
	}
	if d, _ := client.Retryable(&client.APIError{Code: codes.Unavailable}, 10, p); d != 4*time.Second {
		t.Fatalf("backoff is capped: %v", d)
	}
}

func TestRenewsShortLivedCredentials(t *testing.T) {
	srv := start(t, fakelimen.Options{CertTTL: time.Minute})
	srv.AddBotToken("a", client.MethodOIDC, "payments-terraform")
	srv.AddBotToken("b", client.MethodOIDC, "payments-terraform")
	tokens := []string{"a", "b"}
	dir := t.TempDir()
	file := dir + "/token"
	write := func(s string) {
		if err := writeFile(file, s); err != nil {
			t.Fatal(err)
		}
	}
	write(tokens[0])
	c, err := client.New(context.Background(), client.Config{
		Address: srv.Address, CACertPEM: srv.CAPEM,
		Auth:        &client.BotAuth{Method: client.MethodOIDC, TokenFile: file},
		RenewBefore: 2 * time.Minute, // longer than the TTL: every call renews
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	first := c.ExpiresAt()
	write(tokens[1])
	time.Sleep(1100 * time.Millisecond)
	if _, err := c.Manage.DescribeCaller(context.Background(), &managev1.DescribeCallerRequest{}); err != nil {
		t.Fatal(err)
	}
	if !c.ExpiresAt().After(first) {
		t.Fatalf("credentials weren't renewed: %v then %v", first, c.ExpiresAt())
	}
}

func TestGitHubActionsToken(t *testing.T) {
	var gotAuth, gotAudience string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAudience = r.Header.Get("Authorization"), r.URL.Query().Get("audience")
		_, _ = w.Write([]byte(`{"value":"gh-token"}`))
	}))
	defer gh.Close()
	srv := start(t, fakelimen.Options{})
	srv.AddBotToken("gh-token", client.MethodGitHubActions, "payments-terraform")
	env := map[string]string{
		"ACTIONS_ID_TOKEN_REQUEST_URL":   gh.URL + "/token?api-version=2",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runner-secret",
	}
	auth := &client.BotAuth{Method: client.MethodGitHubActions, Bot: "payments-terraform", Getenv: func(k string) string { return env[k] }}
	if !auth.Renewable() {
		t.Fatal("GitHub mints tokens on demand")
	}
	c, err := client.New(context.Background(), client.Config{Address: srv.Address, CACertPEM: srv.CAPEM, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if gotAuth != "Bearer runner-secret" || gotAudience != "limen" {
		t.Fatalf("asked GitHub with %q for audience %q", gotAuth, gotAudience)
	}
	if strings.Contains(auth.Describe(), "runner-secret") {
		t.Fatal("descriptions never contain secrets")
	}
}

func TestGitHubOutsideActionsIsExplained(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	_, err := client.New(context.Background(), client.Config{
		Address: srv.Address, CACertPEM: srv.CAPEM,
		Auth: &client.BotAuth{Method: client.MethodGitHubActions, Getenv: func(string) string { return "" }},
	})
	if err == nil || !strings.Contains(err.Error(), "id-token: write") || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("got %v", err)
	}
}

func TestCertificateAuth(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	srv.AddAdmin("alice")
	cert, key, err := srv.IssueCertificate("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(context.Background(), client.Config{
		Address: srv.Address, CACertPEM: srv.CAPEM, Auth: &client.CertificateAuth{CertPEM: cert, KeyPEM: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Manage.DescribeCaller(context.Background(), &managev1.DescribeCallerRequest{})
	if err != nil || !resp.GetAdmin() {
		t.Fatalf("admin: %v %v", resp, err)
	}
	expired, expiredKey, _ := srv.IssueCertificate("alice", -time.Minute)
	if _, err := (&client.CertificateAuth{CertPEM: expired, KeyPEM: expiredKey}).Login(context.Background(), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired certificates are refused before connecting: %v", err)
	}
}

func TestRequestIDs(t *testing.T) {
	a, b := client.NewRequestID(), client.NewRequestID()
	if a == b || len(a) != 36 || a[14] != '4' {
		t.Fatalf("not random v4 UUIDs: %s %s", a, b)
	}
}

func TestServerName(t *testing.T) {
	for in, want := range map[string]string{"limen.example.com:443": "limen.example.com", "[::1]:3443": "::1"} {
		if got, err := client.ServerNameFor(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"nohost", ":443", "host:"} {
		if _, err := client.ServerNameFor(bad); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

func TestListingRestartsWhenThePolicyChanges(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	owner := &managev1.Ownership{Channel: managev1.Channel_CHANNEL_API, Source: "terraform/a", Namespace: "payments"}
	seed := func(name string) {
		doc := &managev1.Document{Kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE, Name: name, SpecJson: `{"tcp":{"address":"10.0.0.1:1"}}`}
		if err := srv.Seed(doc, owner); err != nil {
			t.Error(err)
		}
	}
	for _, name := range []string{"payments-a", "payments-b", "payments-c"} {
		seed(name)
	}
	c := connectBot(t, srv, "tok-list")
	// The policy changes after the first page was served: the second page's
	// token is then stale, and the listing has to start again.
	changed := 0
	srv.Before("ListDocuments", func() {
		srv.Before("ListDocuments", func() { changed++; seed("payments-d") })
	})
	docs, version, err := c.ListAll(context.Background(), client.ListFilter{Kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 || version != 4 || len(docs) != 4 {
		t.Fatalf("changed=%d version=%d documents=%d", changed, version, len(docs))
	}
	// page 1, page 2 (refused), then four pages of the restarted listing.
	if n := strings.Count(strings.Join(srv.Calls(), ","), "ListDocuments"); n != 6 {
		t.Fatalf("ListDocuments called %d times: %v", n, srv.Calls())
	}
}

func TestUpdateAndDeleteNeedTheOwner(t *testing.T) {
	srv := start(t, fakelimen.Options{})
	c := connectBot(t, srv, "tok-owner")
	srv.Seed(&managev1.Document{Kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE, Name: "payments-postgres", SpecJson: `{"settable":{"postgres.endpoint":{}},"labels":{}}`}, nil) //nolint:errcheck // already seeded in start
	ctx := context.Background()
	doc := &managev1.Document{Kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE, Name: "payments-x", Profile: "payments-postgres", SpecJson: `{"postgres":{"endpoint":"a:1"}}`}
	created, err := c.Manage.CreateDocument(ctx, &managev1.CreateDocumentRequest{Document: doc, Ownership: &managev1.Ownership{Source: "terraform/a", Namespace: "payments"}})
	if err != nil {
		t.Fatal(err)
	}
	etag := created.GetResult().GetDocuments()[0].GetEtag()
	_, err = c.Manage.UpdateDocument(ctx, &managev1.UpdateDocumentRequest{Document: doc, Etag: etag})
	if !client.IsCode(err, codes.InvalidArgument) || !strings.Contains(err.Error(), "ownership.source is required") {
		t.Fatalf("an update without an owner: %v", err)
	}
	_, err = c.Manage.DeleteDocument(ctx, &managev1.DeleteDocumentRequest{Kind: doc.Kind, Name: doc.Name, Etag: etag})
	if !client.IsCode(err, codes.InvalidArgument) {
		t.Fatalf("a delete without an owner: %v", err)
	}
	other := &managev1.Ownership{Source: "terraform/b", Namespace: "payments"}
	_, err = c.Manage.DeleteDocument(ctx, &managev1.DeleteDocumentRequest{Kind: doc.Kind, Name: doc.Name, Etag: etag, Ownership: other})
	if !client.Is(err, client.ReasonOwnedElsewhere) {
		t.Fatalf("another source's delete: %v", err)
	}
}
