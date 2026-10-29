package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	limenv1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/v1"
)

// Credentials is a client certificate and when it stops working.
type Credentials struct {
	Certificate tls.Certificate
	Principal   string
	Roles       []string
	ExpiresAt   time.Time
}

// An Authenticator produces client credentials. It is called once when the
// client connects and again when the credentials are about to expire, if
// Renewable says that can work.
type Authenticator interface {
	// Login returns fresh credentials. auth is an unauthenticated client of
	// Limen's Auth service and cfg its answer to GetAuthConfig.
	Login(ctx context.Context, auth limenv1.AuthClient, cfg *limenv1.GetAuthConfigResponse) (*Credentials, error)
	// Renewable reports whether calling Login again can produce new
	// credentials (a new workload token can be had).
	Renewable() bool
	// Describe names the method for messages, never including a secret.
	Describe() string
}

// ---- Bots ------------------------------------------------------------------

// Bot join methods (ADR-0019).
const (
	MethodGitHubActions = "github_actions"
	MethodGitLab        = "gitlab"
	MethodOIDC          = "oidc"
)

// DefaultTokenEnv is where gitlab and oidc jobs put their token unless told
// otherwise, the same as `limen login --bot`.
const DefaultTokenEnv = "LIMEN_ID_TOKEN"

// BotAuth logs in as a bot with a CI platform's workload identity token.
// No secret is configured: the token exists only for this job, and Limen
// accepts each token once.
type BotAuth struct {
	Method   string
	Bot      string // optional: which bot, when a token could match several
	Audience string // optional: defaults to the server's bot audience

	// Exactly one way to find the token; with none, the method's default
	// (GitHub's token service, or $LIMEN_ID_TOKEN) is used.
	Token     string
	TokenFile string
	TokenEnv  string

	// For tests: GitHub's token service environment and HTTP client.
	Getenv     func(string) string
	HTTPClient *http.Client
}

func (b *BotAuth) getenv(k string) string {
	if b.Getenv != nil {
		return b.Getenv(k)
	}
	return os.Getenv(k)
}

func (b *BotAuth) usesGitHubRuntime() bool {
	return b.Method == MethodGitHubActions && b.Token == "" && b.TokenFile == "" && b.TokenEnv == ""
}

// Renewable: GitHub mints tokens on demand and files can be rotated
// (Kubernetes projected tokens); a literal token or a variable can't change.
func (b *BotAuth) Renewable() bool { return b.usesGitHubRuntime() || b.TokenFile != "" }

func (b *BotAuth) Describe() string {
	name := b.Bot
	if name == "" {
		name = "(matched by token)"
	}
	return fmt.Sprintf("bot %s with %s", name, b.Method)
}

func (b *BotAuth) token(ctx context.Context, audience string) (string, error) {
	var raw string
	switch {
	case b.Token != "":
		raw = b.Token
	case b.TokenFile != "":
		data, err := os.ReadFile(b.TokenFile)
		if err != nil {
			return "", fmt.Errorf("reading the identity token: %w", err)
		}
		raw = string(data)
	case b.usesGitHubRuntime():
		return b.githubToken(ctx, audience)
	default:
		name := b.TokenEnv
		if name == "" {
			name = DefaultTokenEnv
		}
		raw = b.getenv(name)
		if raw == "" {
			return "", fmt.Errorf("the environment variable %s is not set (it should hold the job's identity token)", name)
		}
	}
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", errors.New("the identity token is empty")
	}
	return t, nil
}

// GitHubTokenURL is the URL GitHub Actions serves tokens at, asking for audience.
func GitHubTokenURL(base, audience string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "audience=" + url.QueryEscape(audience)
}

func (b *BotAuth) githubToken(ctx context.Context, audience string) (string, error) {
	base, bearer := b.getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), b.getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if base == "" || bearer == "" {
		return "", errors.New("this doesn't look like a GitHub Actions job with OIDC enabled " +
			"(ACTIONS_ID_TOKEN_REQUEST_URL is not set). Add `permissions: id-token: write` to the job. " +
			"Pull request runs can never log in as a bot: plan them with offline = true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubTokenURL(base, audience), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json; api-version=2.0")
	hc := b.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking GitHub for an identity token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading GitHub's answer: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GitHub refused to issue an identity token (%s)", resp.Status)
	}
	var v struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", errors.New("GitHub's answer wasn't JSON")
	}
	if v.Value == "" {
		return "", errors.New("GitHub's answer had no token")
	}
	return v.Value, nil
}

func (b *BotAuth) Login(ctx context.Context, auth limenv1.AuthClient, cfg *limenv1.GetAuthConfigResponse) (*Credentials, error) {
	audience := b.Audience
	if audience == "" {
		audience = cfg.GetBotAudience()
	}
	if audience == "" {
		audience = "limen"
	}
	token, err := b.token(ctx, audience)
	if err != nil {
		return nil, err
	}
	// The key is generated here and never leaves this process.
	key, csr, err := NewKeyAndCSR()
	if err != nil {
		return nil, err
	}
	resp, err := auth.BotLogin(ctx, &limenv1.BotLoginRequest{
		Method: b.Method,
		Token:  token,
		CsrDer: csr,
		Bot:    b.Bot,
	})
	if err != nil {
		return nil, fmt.Errorf("bot login (%s): %w", b.Describe(), FromStatus(err))
	}
	return credentialsFromLogin(resp, key)
}

// NewKeyAndCSR makes an ECDSA P-256 key, the same as the Limen CLI, and a
// DER PKCS#10 request for it.
func NewKeyAndCSR() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "terraform-provider-limen"},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	return key, csr, nil
}

func credentialsFromLogin(resp *limenv1.LoginResponse, key *ecdsa.PrivateKey) (*Credentials, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair([]byte(resp.GetCertPem()), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return nil, fmt.Errorf("Limen returned an unusable certificate: %w", err)
	}
	return &Credentials{
		Certificate: cert,
		Principal:   resp.GetUsername(),
		Roles:       resp.GetRoles(),
		ExpiresAt:   time.Unix(resp.GetExpiresAtUnix(), 0),
	}, nil
}

// ---- A certificate already issued -------------------------------------------

// CertificateAuth uses a client certificate and key the caller already has,
// for example from `limen login` saved in a secret store for a short job.
type CertificateAuth struct {
	CertPEM string
	KeyPEM  string
}

func (c *CertificateAuth) Renewable() bool  { return false }
func (c *CertificateAuth) Describe() string { return "the configured client certificate" }

func (c *CertificateAuth) Login(context.Context, limenv1.AuthClient, *limenv1.GetAuthConfigResponse) (*Credentials, error) {
	return certificateCredentials(c.CertPEM, c.KeyPEM, "client_certificate")
}

func certificateCredentials(certPEM, keyPEM, what string) (*Credentials, error) {
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("%s expired at %s", what, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return &Credentials{Certificate: cert, Principal: leaf.Subject.CommonName, ExpiresAt: leaf.NotAfter}, nil
}

// ---- The Limen CLI's saved login --------------------------------------------

// LimenHome is $LIMEN_HOME, else ~/.limen, as in the Limen CLI.
func LimenHome() (string, error) {
	if dir := os.Getenv("LIMEN_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("HOME is not set; set LIMEN_HOME")
	}
	return filepath.Join(home, ".limen"), nil
}

// ProfileDir is where the Limen CLI keeps its login for proxy.
func ProfileDir(home, proxy string) string {
	safe := strings.Map(func(r rune) rune {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return r
		}
		return '_'
	}, proxy)
	return filepath.Join(home, "profiles", safe)
}

// CurrentProxy is the proxy the Limen CLI logged in to last, or "".
func CurrentProxy(home string) string {
	data, err := os.ReadFile(filepath.Join(home, "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// PinnedCA is the CA the Limen CLI pinned for proxy, or "".
func PinnedCA(home, proxy string) string {
	data, err := os.ReadFile(filepath.Join(ProfileDir(home, proxy), "proxy-ca.pem"))
	if err != nil {
		return ""
	}
	return string(data)
}

// ProfileAuth reuses the login a person made with `limen login`, for running
// Terraform from a laptop.
type ProfileAuth struct {
	Home  string
	Proxy string
}

func (p *ProfileAuth) Renewable() bool { return false }
func (p *ProfileAuth) Describe() string {
	return fmt.Sprintf("the Limen CLI's login for %s", p.Proxy)
}

type profileMeta struct {
	Username      string   `json:"username"`
	Roles         []string `json:"roles"`
	ExpiresAtUnix int64    `json:"expires_at_unix"`
}

func (p *ProfileAuth) Login(context.Context, limenv1.AuthClient, *limenv1.GetAuthConfigResponse) (*Credentials, error) {
	dir := ProfileDir(p.Home, p.Proxy)
	metaJSON, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("not logged in to %s: run `limen login --proxy %s`, or configure a bot", p.Proxy, p.Proxy)
	} else if err != nil {
		return nil, err
	}
	var meta profileMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, "profile.json"), err)
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, err
	}
	creds, err := certificateCredentials(string(certPEM), string(keyPEM), "the Limen CLI's login for "+p.Proxy)
	if err != nil {
		return nil, fmt.Errorf("%w; run `limen login`", err)
	}
	creds.Principal, creds.Roles = meta.Username, meta.Roles
	return creds, nil
}
