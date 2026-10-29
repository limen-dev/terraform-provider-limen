// Package provider is the Terraform and OpenTofu provider for Limen.
package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

// New returns the provider factory for a version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &limenProvider{version: version} }
}

type limenProvider struct {
	version string
}

type providerModel struct {
	Address           types.String `tfsdk:"address"`
	CACertificate     types.String `tfsdk:"ca_certificate"`
	CAFile            types.String `tfsdk:"ca_file"`
	TLSServerName     types.String `tfsdk:"tls_server_name"`
	Source            types.String `tfsdk:"default_source"`
	Namespace         types.String `tfsdk:"default_namespace"`
	Offline           types.Bool   `tfsdk:"offline"`
	PlanChecks        types.Bool   `tfsdk:"plan_checks"`
	ApprovalTimeout   types.String `tfsdk:"approval_timeout"`
	SourceRef         types.String `tfsdk:"source_ref"`
	ChangeReason      types.String `tfsdk:"change_reason"`
	Bot               *botModel    `tfsdk:"bot"`
	ClientCertificate *certModel   `tfsdk:"client_certificate"`
}

type botModel struct {
	Method    types.String `tfsdk:"method"`
	Name      types.String `tfsdk:"name"`
	Audience  types.String `tfsdk:"audience"`
	Token     types.String `tfsdk:"token"`
	TokenFile types.String `tfsdk:"token_file"`
	TokenEnv  types.String `tfsdk:"token_env"`
}

type certModel struct {
	CertPEM types.String `tfsdk:"cert_pem"`
	KeyPEM  types.String `tfsdk:"key_pem"`
}

func (p *limenProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "limen"
	resp.Version = p.version
}

func (p *limenProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages [Limen](https://github.com/limen-dev/limen) policy documents through its management API (`limen.manage.v1`).\n\n" +
			"In automation, the provider logs in as a Limen **bot** with the CI platform's workload identity token, so no secret is stored anywhere. " +
			"From a laptop, it reuses the login `limen login` saved.",
		Attributes: map[string]schema.Attribute{
			"address": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "A Limen proxy's `host:port`, such as `limen.example.com:443`. " +
					"Defaults to `$LIMEN_ADDRESS`, else the proxy the Limen CLI logged in to last.",
			},
			"ca_certificate": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "PEM of the CA that signs the proxy's certificate. Defaults to `$LIMEN_CA_CERTIFICATE`, " +
					"then the file named by `ca_file`, then the CA the Limen CLI pinned for this proxy.",
			},
			"ca_file": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to a PEM file with the proxy's CA. Defaults to `$LIMEN_CA_FILE`.",
			},
			"tls_server_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The name to verify the proxy's certificate against, when it differs from the address's host.",
			},
			"default_source": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The source documents this configuration creates are owned by, such as `terraform/payments-prod`; " +
					"one per Terraform state is the rule of thumb. Required to create documents. Defaults to `$LIMEN_SOURCE`. " +
					"A source keeps writers that share a namespace from overwriting each other by accident; it is not a security boundary.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 200)},
			},
			"default_namespace": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The namespace documents this configuration creates are owned by. Defaults to `$LIMEN_NAMESPACE`, " +
					"else the bot's grant's namespace, else `default`.",
			},
			"offline": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Plan without contacting Limen. Pull request runs can never log in as a bot (ADR-0019), so plan them offline: " +
					"state isn't refreshed, nothing is dry-run, and apply is refused. Defaults to `$LIMEN_OFFLINE`.",
			},
			"plan_checks": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "During plan, dry-run every change against Limen (`validate_only`): authorization, validation, profiles, tests, " +
					"and who gains or loses access, shown as warnings. Default `true` (`$LIMEN_PLAN_CHECKS`).",
			},
			"approval_timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "How long apply waits for a person to approve a change Limen held because it widens standing access, " +
					"such as `30m`. Default `0s`: fail at once, naming the change; after it is approved, apply again. Defaults to `$LIMEN_APPROVAL_TIMEOUT`.",
			},
			"source_ref": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Where changes come from, recorded in Limen's audit trail, such as a commit or a run URL. " +
					"Defaults to `$LIMEN_SOURCE_REF`, else the GitHub Actions run or GitLab job URL when running there.",
				Validators: []validator.String{stringvalidator.LengthAtMost(200)},
			},
			"change_reason": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Why, in a sentence, shown to approvers of held changes. Defaults to `$LIMEN_CHANGE_REASON`.",
				Validators:          []validator.String{stringvalidator.LengthAtMost(1000)},
			},
			"bot": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Log in as a Limen bot with the job's workload identity token. " +
					"Without this block, `$LIMEN_BOT_METHOD` (and `$LIMEN_BOT_NAME`, `$LIMEN_BOT_AUDIENCE`, `$LIMEN_BOT_TOKEN_FILE`) configure it.",
				Attributes: map[string]schema.Attribute{
					"method": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "The bot's join method: `github_actions`, `gitlab`, or `oidc`.",
						Validators:          []validator.String{stringvalidator.OneOf(client.MethodGitHubActions, client.MethodGitLab, client.MethodOIDC)},
					},
					"name": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "The bot's name, needed only when the token could match more than one bot.",
					},
					"audience": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "The audience to ask GitHub for. Defaults to the control plane's bot audience.",
					},
					"token": schema.StringAttribute{
						Optional:            true,
						Sensitive:           true,
						MarkdownDescription: "The token itself. Prefer `token_env` or `token_file`, so the token isn't in configuration.",
					},
					"token_file": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "A file holding the token, read again when the provider has to log in again (Kubernetes projected tokens).",
					},
					"token_env": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "The environment variable holding the token. Default `LIMEN_ID_TOKEN` for `gitlab` and `oidc`; " +
							"`github_actions` asks GitHub for a token unless a token is named.",
					},
				},
			},
			"client_certificate": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Use a Limen client certificate the caller already has, instead of logging in.",
				Attributes: map[string]schema.Attribute{
					"cert_pem": schema.StringAttribute{Required: true, MarkdownDescription: "The certificate, PEM."},
					"key_pem":  schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "Its private key, PEM."},
				},
			},
		},
	}
}

// providerData is what resources and data sources get from Configure.
type providerData struct {
	client          *client.Client // nil when offline
	offline         bool
	source          string
	namespace       string // configured; "" means ask Limen
	planChecks      bool
	approvalTimeout time.Duration
	sourceRef       string
	changeReason    string

	nsOnce sync.Once
	nsVal  string
	nsErr  error
}

// defaultNamespace is the configured namespace, else the bot's grant's,
// else `default`.
func (d *providerData) defaultNamespace(ctx context.Context) (string, error) {
	if d.namespace != "" {
		return d.namespace, nil
	}
	if d.client == nil {
		return "", nil
	}
	d.nsOnce.Do(func() {
		resp, err := d.client.Manage.DescribeCaller(ctx, &managev1.DescribeCallerRequest{})
		if err != nil {
			d.nsErr = err
			return
		}
		d.nsVal = "default"
		if g := resp.GetGrant(); g != nil && g.GetNamespace() != "" {
			d.nsVal = g.GetNamespace()
		}
	})
	return d.nsVal, d.nsErr
}

func (d *providerData) writeOptions(validateOnly bool) *managev1.WriteOptions {
	o := &managev1.WriteOptions{
		ValidateOnly: validateOnly,
		SourceRef:    d.sourceRef,
		Reason:       d.changeReason,
	}
	if validateOnly {
		o.FailIfApprovalNeeded = true
	} else {
		o.RequestId = client.NewRequestID()
	}
	return o
}

func stringOr(v types.String, env string) string {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueString()
	}
	return os.Getenv(env)
}

func boolOr(v types.Bool, env string, def bool, diags *diag.Diagnostics) bool {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueBool()
	}
	if s := os.Getenv(env); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			diags.AddError("Invalid "+env, fmt.Sprintf("%s=%q is not a boolean", env, s))
			return def
		}
		return b
	}
	return def
}

// defaultSourceRef points at the CI run, when there is one.
func defaultSourceRef() string {
	if repo, run := os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"); repo != "" && run != "" {
		server := os.Getenv("GITHUB_SERVER_URL")
		if server == "" {
			server = "https://github.com"
		}
		return fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, run)
	}
	return os.Getenv("CI_JOB_URL")
}

func (p *limenProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	data := &providerData{
		source:       stringOr(cfg.Source, "LIMEN_SOURCE"),
		namespace:    stringOr(cfg.Namespace, "LIMEN_NAMESPACE"),
		offline:      boolOr(cfg.Offline, "LIMEN_OFFLINE", false, &resp.Diagnostics),
		planChecks:   boolOr(cfg.PlanChecks, "LIMEN_PLAN_CHECKS", true, &resp.Diagnostics),
		sourceRef:    stringOr(cfg.SourceRef, "LIMEN_SOURCE_REF"),
		changeReason: stringOr(cfg.ChangeReason, "LIMEN_CHANGE_REASON"),
	}
	if data.sourceRef == "" {
		data.sourceRef = defaultSourceRef()
	}
	if len(data.sourceRef) > 200 {
		data.sourceRef = data.sourceRef[:200]
	}
	if s := stringOr(cfg.ApprovalTimeout, "LIMEN_APPROVAL_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			resp.Diagnostics.AddAttributeError(path.Root("approval_timeout"), "Invalid approval_timeout",
				fmt.Sprintf("%q is not a duration such as 30m", s))
		}
		data.approvalTimeout = d
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.ResourceData, resp.DataSourceData = data, data
	if data.offline {
		tflog.Info(ctx, "Limen provider is offline: no refresh, no dry runs, no applies")
		return
	}
	// A value that is unknown during plan (from a resource not yet created)
	// means configuration has to wait for apply.
	if cfg.Address.IsUnknown() || cfg.CACertificate.IsUnknown() || cfg.Bot != nil && cfg.Bot.Token.IsUnknown() {
		resp.Diagnostics.AddError("Provider configuration depends on unknown values",
			"The Limen provider's address, CA, and credentials must be known during plan. Configure them from variables or the environment.")
		return
	}

	home, _ := client.LimenHome()
	address := stringOr(cfg.Address, "LIMEN_ADDRESS")
	if address == "" && home != "" {
		address = client.CurrentProxy(home)
	}
	if address == "" {
		resp.Diagnostics.AddAttributeError(path.Root("address"), "No Limen address",
			"Set address (or LIMEN_ADDRESS) to a Limen proxy's host:port, such as limen.example.com:443.")
		return
	}
	caPEM := stringOr(cfg.CACertificate, "LIMEN_CA_CERTIFICATE")
	if caPEM == "" {
		if f := stringOr(cfg.CAFile, "LIMEN_CA_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("ca_file"), "Can't read ca_file", err.Error())
				return
			}
			caPEM = string(b)
		}
	}
	if caPEM == "" && home != "" {
		caPEM = client.PinnedCA(home, address)
	}

	auth, diags := authenticator(cfg, home, address)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	c, err := sharedClient(fmt.Sprintf("%s|%s|%s|%s", address, cfg.TLSServerName.ValueString(), auth.CacheKey(), caPEM), func() (*client.Client, error) {
		return client.New(ctx, client.Config{
			Address:    address,
			CACertPEM:  caPEM,
			ServerName: cfg.TLSServerName.ValueString(),
			Auth:       auth,
			// The CLI's version too, for whoever reads the control plane's logs.
			UserAgent: fmt.Sprintf("terraform-provider-limen/%s (terraform/%s)", p.version, req.TerraformVersion),
			Logf:      func(f string, a ...any) { tflog.Debug(ctx, fmt.Sprintf(f, a...)) },
		})
	})
	if err != nil {
		summary, detail := client.Explain(err)
		resp.Diagnostics.AddError(summary, fmt.Sprintf("Logging in to %s with %s:\n\n%s", address, auth.Describe(), detail))
		return
	}
	data.client = c
	tflog.Info(ctx, "logged in to Limen", map[string]any{
		"address": address, "principal": c.Principal(), "server_version": c.ServerVersion(),
		"expires_at": c.ExpiresAt().UTC().Format(time.RFC3339),
	})
}

// logins holds the clients this process has logged in, by address and
// credentials. Terraform configures the provider more than once in a single
// run (the plan and apply walks of `terraform apply`), and a bot's workload
// token works only once: a second login with the same token would be refused
// as a replay. Reusing the certificate is also what saves a login.
var logins = struct {
	sync.Mutex
	clients map[string]*client.Client
}{clients: map[string]*client.Client{}}

func sharedClient(key string, login func() (*client.Client, error)) (*client.Client, error) {
	logins.Lock()
	defer logins.Unlock()
	if c := logins.clients[key]; c != nil && c.Reusable() {
		return c, nil
	}
	c, err := login()
	if err != nil {
		return nil, err
	}
	logins.clients[key] = c
	return c, nil
}

// authenticator picks the credentials: a bot (block or environment), then a
// client certificate, then the Limen CLI's saved login.
func authenticator(cfg providerModel, home, address string) (client.Authenticator, diag.Diagnostics) {
	var diags diag.Diagnostics
	bot := cfg.Bot
	if bot == nil && os.Getenv("LIMEN_BOT_METHOD") != "" {
		bot = &botModel{
			Method:    types.StringValue(os.Getenv("LIMEN_BOT_METHOD")),
			Name:      envString("LIMEN_BOT_NAME"),
			Audience:  envString("LIMEN_BOT_AUDIENCE"),
			TokenFile: envString("LIMEN_BOT_TOKEN_FILE"),
			Token:     types.StringNull(),
			TokenEnv:  types.StringNull(),
		}
	}
	if bot != nil && cfg.ClientCertificate != nil {
		diags.AddError("Two kinds of credentials", "Configure either bot or client_certificate, not both.")
		return nil, diags
	}
	if bot != nil {
		set := 0
		for _, v := range []types.String{bot.Token, bot.TokenFile, bot.TokenEnv} {
			if v.ValueString() != "" {
				set++
			}
		}
		if set > 1 {
			diags.AddAttributeError(path.Root("bot"), "Too many token sources", "Set at most one of token, token_file, and token_env.")
			return nil, diags
		}
		method := bot.Method.ValueString()
		switch method {
		case client.MethodGitHubActions, client.MethodGitLab, client.MethodOIDC:
		default:
			diags.AddAttributeError(path.Root("bot").AtName("method"), "Unknown bot method",
				fmt.Sprintf("%q: use github_actions, gitlab, or oidc", method))
			return nil, diags
		}
		return &client.BotAuth{
			Method:    method,
			Bot:       bot.Name.ValueString(),
			Audience:  bot.Audience.ValueString(),
			Token:     bot.Token.ValueString(),
			TokenFile: bot.TokenFile.ValueString(),
			TokenEnv:  bot.TokenEnv.ValueString(),
		}, diags
	}
	if cc := cfg.ClientCertificate; cc != nil {
		return &client.CertificateAuth{CertPEM: cc.CertPEM.ValueString(), KeyPEM: cc.KeyPEM.ValueString()}, diags
	}
	if home == "" {
		diags.AddError("No Limen credentials", "Configure bot or client_certificate, or log in with `limen login`.")
		return nil, diags
	}
	return &client.ProfileAuth{Home: home, Proxy: address}, diags
}

func envString(name string) types.String {
	if v := os.Getenv(name); v != "" {
		return types.StringValue(v)
	}
	return types.StringNull()
}

func (p *limenProvider) Resources(context.Context) []func() resource.Resource {
	var out []func() resource.Resource
	for _, k := range writableKinds {
		out = append(out, newDocumentResource(k))
	}
	return out
}

func (p *limenProvider) DataSources(context.Context) []func() datasource.DataSource {
	out := []func() datasource.DataSource{
		newCallerDataSource,
		newDocumentsDataSource,
		newSchemaDataSource,
	}
	for _, k := range allKinds {
		out = append(out, newDocumentDataSource(k))
	}
	return out
}
