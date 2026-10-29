// Package client is the provider's connection to Limen: mutual TLS to a
// proxy, a bot login or another credential, and the management API
// (`limen.manage.v1`) with retries that are safe because every write carries
// a request id.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"

	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
	limenv1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/v1"
)

// Config says how to reach Limen.
type Config struct {
	// Address is a proxy's host:port.
	Address string
	// CACertPEM is the CA that signs the proxy's certificate.
	CACertPEM string
	// ServerName overrides the TLS server name (default: the address's host).
	ServerName string
	// Auth produces the client certificate.
	Auth Authenticator
	// UserAgent is sent with every call.
	UserAgent string
	// Retry bounds the retries of one call.
	Retry RetryPolicy
	// RenewBefore is how long before expiry credentials are renewed, when the
	// authenticator can renew them. Default 2 minutes.
	RenewBefore time.Duration
	// Logf receives debug messages; never secrets.
	Logf func(format string, args ...any)
}

// Client is a connected, authenticated client of Limen.
type Client struct {
	Manage managev1.ManagementServiceClient
	Auth   limenv1.AuthClient

	cfg        Config
	tls        *tls.Config
	authConfig *limenv1.GetAuthConfigResponse

	mu    sync.Mutex
	conn  *grpc.ClientConn
	creds *Credentials
}

// ServerVersion is the control plane's version, from GetAuthConfig.
func (c *Client) ServerVersion() string { return c.authConfig.GetServerVersion() }

// Principal is who the client is logged in as: a person's name or `bot:<name>`.
func (c *Client) Principal() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creds.Principal
}

// ExpiresAt is when the current credentials stop working.
func (c *Client) ExpiresAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creds.ExpiresAt
}

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// ServerNameFor is the host part of host:port, used as the TLS server name.
func ServerNameFor(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("address must be host:port, such as limen.example.com:443: %w", err)
	}
	if host == "" || port == "" {
		return "", errors.New("address must be host:port, such as limen.example.com:443")
	}
	return host, nil
}

// New connects to Limen and logs in.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Auth == nil {
		return nil, errors.New("no credentials configured")
	}
	if cfg.RenewBefore == 0 {
		cfg.RenewBefore = 2 * time.Minute
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.Retry = cfg.Retry.withDefaults()
	serverName := cfg.ServerName
	if serverName == "" {
		var err error
		if serverName, err = ServerNameFor(cfg.Address); err != nil {
			return nil, err
		}
	}
	roots := x509.NewCertPool()
	if cfg.CACertPEM == "" {
		return nil, errors.New("no CA certificate for the proxy: set ca_certificate or ca_file " +
			"(or log in once with `limen login --ca-file`, which pins it)")
	}
	if !roots.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
		return nil, errors.New("the CA certificate is not PEM")
	}
	c := &Client{
		cfg: cfg,
		tls: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: serverName,
			NextProtos: []string{"h2"},
		},
	}
	shared := &sharedConn{c: c}
	c.Manage = managev1.NewManagementServiceClient(shared)
	c.Auth = limenv1.NewAuthClient(shared)

	// Log in over a connection without a client certificate: BotLogin and
	// GetAuthConfig are unauthenticated.
	anon, err := c.dial(nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = anon.Close() }()
	anonAuth := limenv1.NewAuthClient(anon)
	if c.authConfig, err = anonAuth.GetAuthConfig(ctx, &limenv1.GetAuthConfigRequest{}); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cfg.Address, FromStatus(err))
	}
	creds, err := cfg.Auth.Login(ctx, anonAuth, c.authConfig)
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(&creds.Certificate)
	if err != nil {
		return nil, err
	}
	c.conn, c.creds = conn, creds
	cfg.Logf("logged in to %s as %s (%s), until %s", cfg.Address, creds.Principal, cfg.Auth.Describe(), creds.ExpiresAt.UTC().Format(time.RFC3339))
	return c, nil
}

func (c *Client) dial(cert *tls.Certificate) (*grpc.ClientConn, error) {
	tlsCfg := c.tls.Clone()
	if cert != nil {
		tlsCfg.Certificates = []tls.Certificate{*cert}
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithChainUnaryInterceptor(c.retryInterceptor),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16 << 20)),
	}
	if c.cfg.UserAgent != "" {
		opts = append(opts, grpc.WithUserAgent(c.cfg.UserAgent))
	}
	return grpc.NewClient("passthrough:///"+c.cfg.Address, opts...)
}

// current returns the authenticated connection, logging in again first when
// the credentials are about to expire and can be renewed.
func (c *Client) current(ctx context.Context) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := time.Until(c.creds.ExpiresAt)
	if remaining > c.cfg.RenewBefore {
		return c.conn, nil
	}
	if !c.cfg.Auth.Renewable() {
		if remaining <= 0 {
			return nil, fmt.Errorf("the Limen credentials (%s) expired at %s and can't be renewed: "+
				"bot certificates last at most an hour, so split long applies or use a token source "+
				"that can mint a new token (GitHub Actions, or token_file)",
				c.cfg.Auth.Describe(), c.creds.ExpiresAt.UTC().Format(time.RFC3339))
		}
		return c.conn, nil
	}
	anon, err := c.dial(nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = anon.Close() }()
	creds, err := c.cfg.Auth.Login(ctx, limenv1.NewAuthClient(anon), c.authConfig)
	if err != nil {
		return nil, fmt.Errorf("renewing Limen credentials: %w", err)
	}
	conn, err := c.dial(&creds.Certificate)
	if err != nil {
		return nil, err
	}
	old := c.conn
	c.conn, c.creds = conn, creds
	c.cfg.Logf("renewed Limen credentials for %s until %s", creds.Principal, creds.ExpiresAt.UTC().Format(time.RFC3339))
	// In-flight calls on the old connection finish on their own.
	go func() { time.Sleep(time.Minute); _ = old.Close() }()
	return conn, nil
}

// sharedConn sends every call over the client's current connection, so the
// generated clients survive a renewal.
type sharedConn struct{ c *Client }

func (s *sharedConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	conn, err := s.c.current(ctx)
	if err != nil {
		return err
	}
	return FromStatus(conn.Invoke(ctx, method, args, reply, opts...))
}

func (s *sharedConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	conn, err := s.c.current(ctx)
	if err != nil {
		return nil, err
	}
	return conn.NewStream(ctx, desc, method, opts...)
}

// ---- Retries ------------------------------------------------------------------

// RetryPolicy bounds the retries of one call.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// Sleep waits; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 6
	}
	if p.BaseDelay == 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay == 0 {
		p.MaxDelay = 30 * time.Second
	}
	if p.Sleep == nil {
		p.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return p
}

// Retryable reports whether a call that failed with err may be sent again
// unchanged, and how long to wait first. Reads are always safe to repeat;
// writes are because they carry a request id, which makes Limen return the
// first result instead of writing twice.
func Retryable(err error, attempt int, p RetryPolicy) (time.Duration, bool) {
	var apiErr *APIError
	if !errors.As(FromStatus(err), &apiErr) {
		return 0, false
	}
	backoff := p.BaseDelay << (attempt - 1)
	if backoff > p.MaxDelay || backoff <= 0 {
		backoff = p.MaxDelay
	}
	switch apiErr.Code {
	case codes.Unavailable:
		// A TLS or address problem won't fix itself.
		if strings.Contains(apiErr.Message, "authentication handshake failed") || transportHint(apiErr.Message) != "" &&
			!strings.Contains(apiErr.Message, "connection refused") {
			return 0, false
		}
		return backoff, true
	case codes.ResourceExhausted:
		if apiErr.Reason != ReasonRateLimited {
			return 0, false
		}
		if apiErr.RetryAfter > 0 {
			return min(apiErr.RetryAfter, p.MaxDelay), true
		}
		return backoff, true
	case codes.Aborted:
		return backoff, true
	}
	return 0, false
}

func (c *Client) retryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	p := c.cfg.Retry
	for attempt := 1; ; attempt++ {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err == nil || attempt >= p.MaxAttempts {
			return err
		}
		wait, ok := Retryable(err, attempt, p)
		if !ok {
			return err
		}
		c.cfg.Logf("%s failed (%v); retry %d in %s", method, FromStatus(err), attempt, wait)
		if serr := p.Sleep(ctx, wait); serr != nil {
			return err
		}
	}
}
