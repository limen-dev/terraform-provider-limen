// Package fakelimen is an in-memory stand-in for Limen's management API and
// bot login, for the provider's tests and for trying the provider locally.
//
// It serves the real `limen.manage.v1` and `limen.v1.Auth` contracts over
// mutual TLS and follows ADR-0042's rules where the provider can observe
// them: etags, one owner per document, writer grants, people-only kinds,
// profiles with settable fields and label bounds, idempotent request ids,
// dry runs, held changes, and structured errors. It is not Limen: policy
// semantics (roles, access evaluation, tests) are simulated, and anything
// verified only against it is unverified against a real control plane.
//
// One deliberate quirk mirrors Limen's normalization: a spec is stored with
// keys sorted and `false` values left out (as if `false` were every boolean's
// default), so tests can check that the provider shows no phantom diff.
package fakelimen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
	limenv1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/v1"
)

// Writer is a bot's `policy_writer` grant.
type Writer struct {
	Name           string
	Namespace      string
	NamePatterns   []string
	Kinds          []managev1.DocumentKind
	Profiles       []string
	MaxDocuments   uint32
	ApproverGroups []string
}

// Options configure a fake server.
type Options struct {
	// Address to listen on; default 127.0.0.1:0.
	Address string
	// ServerVersion reported by GetAuthConfig; default "0.14.0-fake".
	ServerVersion string
	// CertTTL of issued bot certificates; default one hour.
	CertTTL time.Duration
}

// Server is a running fake.
type Server struct {
	// Address is host:port of the fake proxy.
	Address string
	// CAPEM is the CA to trust for it.
	CAPEM string

	opts Options
	pki  *pki
	grpc *grpc.Server
	lis  net.Listener

	mu         sync.Mutex
	docs       map[docKey]*managev1.Document
	version    int64
	versions   []*managev1.PolicyVersion
	changes    map[string]*managev1.PolicyChange
	changeSeq  int
	requests   map[string]requestRecord
	admins     map[string]bool
	writers    map[string]*Writer // by bot name
	tokens     map[string]botToken
	usedTokens map[string]bool
	holdKinds  map[managev1.DocumentKind]bool
	faults     []*fault
	calls      []string
}

type docKey struct {
	kind managev1.DocumentKind
	name string
}

type botToken struct{ method, bot string }

type fault struct {
	method     string
	code       codes.Code
	reason     string
	retryAfter time.Duration
	remaining  int
}

// Start starts a fake server. Stop it with Close.
func Start(opts Options) (*Server, error) {
	if opts.Address == "" {
		opts.Address = "127.0.0.1:0"
	}
	if opts.ServerVersion == "" {
		opts.ServerVersion = "0.14.0-fake"
	}
	if opts.CertTTL == 0 {
		opts.CertTTL = time.Hour
	}
	p, err := newPKI()
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := p.serverCert()
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(p.caCert)
	lis, err := net.Listen("tcp", opts.Address)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Address:    lis.Addr().String(),
		CAPEM:      p.caPEM,
		opts:       opts,
		pki:        p,
		lis:        lis,
		docs:       map[docKey]*managev1.Document{},
		changes:    map[string]*managev1.PolicyChange{},
		requests:   map[string]requestRecord{},
		admins:     map[string]bool{},
		writers:    map[string]*Writer{},
		tokens:     map[string]botToken{},
		usedTokens: map[string]bool{},
		holdKinds:  map[managev1.DocumentKind]bool{},
	}
	s.grpc = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    clientCAs,
		})),
		grpc.UnaryInterceptor(s.intercept),
	)
	managev1.RegisterManagementServiceServer(s.grpc, &manageServer{s: s})
	limenv1.RegisterAuthServer(s.grpc, &authServer{s: s})
	go func() { _ = s.grpc.Serve(lis) }()
	return s, nil
}

// Close stops the server.
func (s *Server) Close() { s.grpc.Stop() }

// ---- Setup, for tests ---------------------------------------------------------

// AddAdmin makes principal a person with `limen:admin`.
func (s *Server) AddAdmin(principal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admins[principal] = true
}

// AddWriter registers the bot named bot as a writer with grant w.
func (s *Server) AddWriter(bot string, w Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := w
	s.writers[bot] = &cp
}

// AddBotToken makes token log a bot in once with method.
func (s *Server) AddBotToken(token, method, bot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = botToken{method: method, bot: bot}
}

// HoldWritesTo makes every bot write to kind wait for approval, as if it
// widened standing access.
func (s *Server) HoldWritesTo(kind managev1.DocumentKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdKinds[kind] = true
}

// FailNext makes the next n calls of method (the RPC name, such as
// "CreateDocument") fail with code and reason, before doing anything.
func (s *Server) FailNext(method string, n int, code codes.Code, reason string, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{method: method, code: code, reason: reason, retryAfter: retryAfter, remaining: n})
}

// Calls lists the RPC names called so far, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// IssueCertificate returns a client certificate and key for principal,
// valid for ttl.
func (s *Server) IssueCertificate(principal string, ttl time.Duration) (certPEM, keyPEM string, err error) {
	key, csr, err := newKey()
	if err != nil {
		return "", "", err
	}
	_ = csr
	cert, _, err := s.pki.clientCert(&key.PublicKey, principal, ttl)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(cert), string(pemBlock("PRIVATE KEY", keyDER)), nil
}

// Seed stores a document as if a person had applied it with `limen admin
// apply` (channel git, source and namespace default) unless own says otherwise.
func (s *Server) Seed(doc *managev1.Document, own *managev1.Ownership) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if own == nil {
		own = &managev1.Ownership{Channel: managev1.Channel_CHANNEL_GIT, Source: "default", Namespace: "default"}
	}
	d := cloneDoc(doc)
	d.Ownership = cloneOwnership(own)
	if err := s.complete(d, "seed"); err != nil {
		return err
	}
	s.version++
	s.stamp(d, "admin")
	s.docs[docKey{d.Kind, d.Name}] = d
	s.versions = append(s.versions, &managev1.PolicyVersion{Version: s.version, Author: "admin", Ownership: d.Ownership, CreateTimeUnix: time.Now().Unix(), Created: []string{kindName(d.Kind) + "/" + d.Name}})
	return nil
}

// Document returns a copy of the stored document, or nil.
func (s *Server) Document(kind managev1.DocumentKind, name string) *managev1.Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.docs[docKey{kind, name}]; d != nil {
		return cloneDoc(d)
	}
	return nil
}

// EditDocument changes a stored document out of band, as another channel
// would, giving it a new etag.
func (s *Server) EditDocument(kind managev1.DocumentKind, name string, edit func(*managev1.Document)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.docs[docKey{kind, name}]
	if d == nil {
		return fmt.Errorf("no %s/%s", kindName(kind), name)
	}
	edit(d)
	if err := s.complete(d, "edit"); err != nil {
		return err
	}
	s.version++
	s.stamp(d, "someone-else")
	return nil
}

// DeleteDocument removes a stored document out of band.
func (s *Server) DeleteDocument(kind managev1.DocumentKind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, docKey{kind, name})
	s.version++
}

// PendingChanges lists the ids of changes waiting for approval.
func (s *Server) PendingChanges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, c := range s.changes {
		if c.State == managev1.PolicyChange_STATE_PENDING {
			ids = append(ids, id)
		}
	}
	return ids
}

// Decide approves (applying it) or rejects a pending change, as a person.
func (s *Server) Decide(id string, approve bool, by, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.decide(id, approve, by, note)
	return err
}

// ---- Interceptor ----------------------------------------------------------------

var unauthenticated = map[string]bool{
	"/limen.v1.Auth/GetAuthConfig": true,
	"/limen.v1.Auth/BotLogin":      true,
}

type principalKey struct{}

func (s *Server) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
	s.mu.Lock()
	s.calls = append(s.calls, method)
	for _, f := range s.faults {
		if f.remaining > 0 && f.method == method {
			f.remaining--
			s.mu.Unlock()
			return nil, apiError(f.code, f.reason, "injected failure", map[string]string{}, f.retryAfter)
		}
	}
	s.mu.Unlock()
	if !unauthenticated[info.FullMethod] {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no peer")
		}
		ti, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(ti.State.PeerCertificates) == 0 {
			return nil, status.Error(codes.Unauthenticated, "a client certificate is required")
		}
		leaf := ti.State.PeerCertificates[0]
		if time.Now().After(leaf.NotAfter) {
			return nil, status.Error(codes.Unauthenticated, "certificate expired")
		}
		ctx = context.WithValue(ctx, principalKey{}, leaf.Subject.CommonName)
	}
	return handler(ctx, req)
}

func principalFrom(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

// ---- Auth -----------------------------------------------------------------------

type authServer struct {
	limenv1.UnimplementedAuthServer
	s *Server
}

func (a *authServer) GetAuthConfig(context.Context, *limenv1.GetAuthConfigRequest) (*limenv1.GetAuthConfigResponse, error) {
	return &limenv1.GetAuthConfigResponse{
		IssuerUrl:     "https://idp.example.com",
		ClientId:      "limen-cli",
		Scopes:        []string{"openid", "email", "groups"},
		ServerVersion: a.s.opts.ServerVersion,
		BotAudience:   "limen",
	}, nil
}

func (a *authServer) BotLogin(_ context.Context, req *limenv1.BotLoginRequest) (*limenv1.LoginResponse, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.tokens[req.GetToken()]
	if !ok || tok.method != req.GetMethod() {
		return nil, status.Error(codes.Unauthenticated, "the token matches no bot")
	}
	if req.GetBot() != "" && req.GetBot() != tok.bot {
		return nil, status.Error(codes.Unauthenticated, "the token doesn't match that bot")
	}
	if s.usedTokens[req.GetToken()] {
		return nil, status.Error(codes.Unauthenticated, "this token was already used")
	}
	csr, err := x509.ParseCertificateRequest(req.GetCsrDer())
	if err != nil || csr.CheckSignature() != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid CSR")
	}
	s.usedTokens[req.GetToken()] = true
	principal := "bot:" + tok.bot
	cert, notAfter, err := s.pki.clientCert(csr.PublicKey, principal, s.opts.CertTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &limenv1.LoginResponse{
		Username:      principal,
		LoginId:       fmt.Sprintf("login-%d", time.Now().UnixNano()),
		CertPem:       string(cert),
		UserCaPem:     s.CAPEM,
		InternalCaPem: s.CAPEM,
		ExpiresAtUnix: notAfter.Unix(),
	}, nil
}

func (a *authServer) WhoAmI(ctx context.Context, _ *limenv1.WhoAmIRequest) (*limenv1.WhoAmIResponse, error) {
	return &limenv1.WhoAmIResponse{Username: principalFrom(ctx)}, nil
}
