package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
	"github.com/limen-dev/terraform-provider-limen/internal/specjson"
)

// base is shared by every data source.
type base struct{ data *providerData }

func (b *base) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("%T", req.ProviderData))
		return
	}
	b.data = data
}

func (b *base) online(diags *diag.Diagnostics, what string) *client.Client {
	if b.data == nil {
		diags.AddError("Provider not configured", "The Limen provider wasn't configured before reading "+what+".")
		return nil
	}
	if b.data.client == nil {
		diags.AddError("Limen provider is offline",
			"Data sources read Limen, so they can't be used with offline = true. Pull request plans run offline: "+
				"pass what they need as variables, or read it in a configuration that plans online.")
		return nil
	}
	return b.data.client
}

// ---- One document: limen_<kind> ---------------------------------------------------

type documentDataSource struct {
	base
	k kindInfo
}

func newDocumentDataSource(k kindInfo) func() datasource.DataSource {
	return func() datasource.DataSource { return &documentDataSource{k: k} }
}

type documentDataModel struct {
	Name              types.String  `tfsdk:"name"`
	PolicyVersion     types.Int64   `tfsdk:"policy_version"`
	ID                types.String  `tfsdk:"id"`
	Description       types.String  `tfsdk:"description"`
	Labels            types.Map     `tfsdk:"labels"`
	Spec              types.Dynamic `tfsdk:"spec"`
	SpecJSON          types.String  `tfsdk:"spec_json"`
	Profile           types.String  `tfsdk:"profile"`
	EffectiveSpec     types.Dynamic `tfsdk:"effective_spec"`
	EffectiveSpecJSON types.String  `tfsdk:"effective_spec_json"`
	EffectiveLabels   types.Map     `tfsdk:"effective_labels"`
	Source            types.String  `tfsdk:"source"`
	Namespace         types.String  `tfsdk:"namespace"`
	Channel           types.String  `tfsdk:"channel"`
	Etag              types.String  `tfsdk:"etag"`
	CreatedAt         types.String  `tfsdk:"created_at"`
	UpdatedAt         types.String  `tfsdk:"updated_at"`
	UpdatedBy         types.String  `tfsdk:"updated_by"`
}

func (d *documentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + d.k.name
}

func (d *documentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computed := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads one " + d.k.what + ". " + d.k.doc,
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "The " + d.k.what + "'s name."},
			"policy_version": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Read from this past policy version instead of the active one. Afterwards, the version the revision read first appeared in.",
			},
			"id":          computed("The name."),
			"description": computed("Free text."),
			"labels":      schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The labels its writer set."},
			"spec": schema.DynamicAttribute{
				Computed:            true,
				MarkdownDescription: "The spec its writer set, as a value: `.spec.postgres.endpoint` and so on.",
			},
			"spec_json":           computed("The same, as normalized JSON."),
			"profile":             computed("For a resource, the profile it completes; otherwise empty."),
			"effective_spec":      schema.DynamicAttribute{Computed: true, MarkdownDescription: "What enforcement uses, as a value."},
			"effective_spec_json": computed("What enforcement uses, as JSON."),
			"effective_labels":    schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "What roles select on."},
			"source":              computed("The owner's source."),
			"namespace":           computed("The owner's namespace."),
			"channel":             computed("How its owner writes it: `git`, `console`, or `api`."),
			"etag":                computed("Changes whenever the document or its owner changes."),
			"created_at":          computed("When it was created (RFC 3339)."),
			"updated_at":          computed("When this revision was written (RFC 3339)."),
			"updated_by":          computed("Who wrote this revision."),
		},
	}
}

func (d *documentDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m documentDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c := d.online(&resp.Diagnostics, "limen_"+d.k.name)
	if c == nil {
		return
	}
	name := m.Name.ValueString()
	out, err := c.Manage.GetDocument(ctx, &managev1.GetDocumentRequest{Kind: d.k.kind, Name: name, PolicyVersion: m.PolicyVersion.ValueInt64()})
	if err != nil {
		addError(&resp.Diagnostics, fmt.Sprintf("Reading %s/%s", d.k.name, name), err)
		return
	}
	doc := out.GetDocument()
	spec, err := specjson.ToValue([]byte(doc.GetSpecJson()))
	if err != nil {
		resp.Diagnostics.AddError("Limen returned a spec that isn't JSON", err.Error())
		return
	}
	eff, err := specjson.ToValue([]byte(doc.GetEffectiveSpecJson()))
	if err != nil {
		resp.Diagnostics.AddError("Limen returned an effective spec that isn't JSON", err.Error())
		return
	}
	m.ID = types.StringValue(doc.GetName())
	m.PolicyVersion = types.Int64Value(doc.GetPolicyVersion())
	m.Description = types.StringValue(doc.GetDescription())
	m.Labels = stringMap(doc.GetLabels())
	m.Spec, m.SpecJSON = spec, types.StringValue(doc.GetSpecJson())
	m.Profile = types.StringValue(doc.GetProfile())
	m.EffectiveSpec, m.EffectiveSpecJSON = eff, types.StringValue(doc.GetEffectiveSpecJson())
	m.EffectiveLabels = stringMap(doc.GetEffectiveLabels())
	m.Source = types.StringValue(doc.GetOwnership().GetSource())
	m.Namespace = types.StringValue(doc.GetOwnership().GetNamespace())
	m.Channel = types.StringValue(client.ChannelName(doc.GetOwnership().GetChannel()))
	m.Etag = types.StringValue(doc.GetEtag())
	m.CreatedAt = unixTime(doc.GetCreateTimeUnix())
	m.UpdatedAt = unixTime(doc.GetUpdateTimeUnix())
	m.UpdatedBy = types.StringValue(doc.GetUpdatedBy())
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---- Many documents: limen_documents ----------------------------------------------

type documentsDataSource struct{ base }

func newDocumentsDataSource() datasource.DataSource { return &documentsDataSource{} }

type documentsModel struct {
	Kind          types.String `tfsdk:"kind"`
	Namespace     types.String `tfsdk:"namespace"`
	Source        types.String `tfsdk:"source"`
	Labels        types.Map    `tfsdk:"labels"`
	Names         types.List   `tfsdk:"names"`
	Documents     types.List   `tfsdk:"documents"`
	PolicyVersion types.Int64  `tfsdk:"policy_version"`
}

var listedDocumentType = map[string]attr.Type{
	"name":                types.StringType,
	"description":         types.StringType,
	"labels":              types.MapType{ElemType: types.StringType},
	"effective_labels":    types.MapType{ElemType: types.StringType},
	"profile":             types.StringType,
	"spec_json":           types.StringType,
	"effective_spec_json": types.StringType,
	"source":              types.StringType,
	"namespace":           types.StringType,
	"channel":             types.StringType,
	"etag":                types.StringType,
	"policy_version":      types.Int64Type,
	"updated_at":          types.StringType,
	"updated_by":          types.StringType,
}

func (d *documentsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_documents"
}

func (d *documentsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists policy documents of one kind, in name order, all from one policy version even while others write. " +
			"A bot writer sees only its own namespace (and the profiles its grant names).",
		Attributes: map[string]schema.Attribute{
			"kind": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The kind: one of `" + joinKinds() + "`.",
				Validators:          []validator.String{stringvalidator.OneOf(client.KindNames...)},
			},
			"namespace": schema.StringAttribute{Optional: true, MarkdownDescription: "Only documents in this namespace."},
			"source":    schema.StringAttribute{Optional: true, MarkdownDescription: "Only documents owned by this source."},
			"labels": schema.MapAttribute{
				Optional: true, ElementType: types.StringType,
				MarkdownDescription: "Only documents carrying all of these labels (matched against their effective labels).",
			},
			"names":          schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The documents' names, in order."},
			"policy_version": schema.Int64Attribute{Computed: true, MarkdownDescription: "The policy version the listing was read from."},
			"documents": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The documents, in name order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":                str("The name."),
					"description":         str("Free text."),
					"labels":              schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The labels its writer set."},
					"effective_labels":    schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "What roles select on."},
					"profile":             str("For a resource, the profile it completes."),
					"spec_json":           str("The spec its writer set, as JSON."),
					"effective_spec_json": str("What enforcement uses, as JSON."),
					"source":              str("The owner's source."),
					"namespace":           str("The owner's namespace."),
					"channel":             str("`git`, `console`, or `api`."),
					"etag":                str("The current etag."),
					"policy_version":      schema.Int64Attribute{Computed: true, MarkdownDescription: "The version this revision first appeared in."},
					"updated_at":          str("When this revision was written."),
					"updated_by":          str("Who wrote this revision."),
				}},
			},
		},
	}
}

func joinKinds() string {
	s := ""
	for i, k := range client.KindNames {
		if i > 0 {
			s += "`, `"
		}
		s += k
	}
	return s
}

func (d *documentsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m documentsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c := d.online(&resp.Diagnostics, "limen_documents")
	if c == nil {
		return
	}
	kind, err := client.ParseKind(m.Kind.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unknown kind", err.Error())
		return
	}
	labels := map[string]string{}
	resp.Diagnostics.Append(m.Labels.ElementsAs(ctx, &labels, false)...)
	docs, version, err := c.ListAll(ctx, client.ListFilter{Kind: kind, Namespace: m.Namespace.ValueString(), Source: m.Source.ValueString(), Labels: labels})
	if err != nil {
		addError(&resp.Diagnostics, "Listing "+m.Kind.ValueString()+" documents", err)
		return
	}
	slices.SortFunc(docs, func(a, b *managev1.Document) int {
		switch {
		case a.GetName() < b.GetName():
			return -1
		case a.GetName() > b.GetName():
			return 1
		}
		return 0
	})
	names := make([]attr.Value, 0, len(docs))
	objs := make([]attr.Value, 0, len(docs))
	objType := types.ObjectType{AttrTypes: listedDocumentType}
	for _, doc := range docs {
		names = append(names, types.StringValue(doc.GetName()))
		o, diags := types.ObjectValue(listedDocumentType, map[string]attr.Value{
			"name":                types.StringValue(doc.GetName()),
			"description":         types.StringValue(doc.GetDescription()),
			"labels":              stringMap(doc.GetLabels()),
			"effective_labels":    stringMap(doc.GetEffectiveLabels()),
			"profile":             types.StringValue(doc.GetProfile()),
			"spec_json":           types.StringValue(doc.GetSpecJson()),
			"effective_spec_json": types.StringValue(doc.GetEffectiveSpecJson()),
			"source":              types.StringValue(doc.GetOwnership().GetSource()),
			"namespace":           types.StringValue(doc.GetOwnership().GetNamespace()),
			"channel":             types.StringValue(client.ChannelName(doc.GetOwnership().GetChannel())),
			"etag":                types.StringValue(doc.GetEtag()),
			"policy_version":      types.Int64Value(doc.GetPolicyVersion()),
			"updated_at":          unixTime(doc.GetUpdateTimeUnix()),
			"updated_by":          types.StringValue(doc.GetUpdatedBy()),
		})
		resp.Diagnostics.Append(diags...)
		objs = append(objs, o)
	}
	m.Names = types.ListValueMust(types.StringType, names)
	m.Documents = types.ListValueMust(objType, objs)
	m.PolicyVersion = types.Int64Value(version)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---- Who am I: limen_caller ------------------------------------------------------

type callerDataSource struct{ base }

func newCallerDataSource() datasource.DataSource { return &callerDataSource{} }

type callerModel struct {
	Principal     types.String `tfsdk:"principal"`
	Admin         types.Bool   `tfsdk:"admin"`
	PolicyVersion types.Int64  `tfsdk:"policy_version"`
	ServerVersion types.String `tfsdk:"server_version"`
	ExpiresAt     types.String `tfsdk:"expires_at"`
	Grant         types.Object `tfsdk:"grant"`
}

var grantType = map[string]attr.Type{
	"name":            types.StringType,
	"namespace":       types.StringType,
	"name_patterns":   types.ListType{ElemType: types.StringType},
	"kinds":           types.ListType{ElemType: types.StringType},
	"profiles":        types.ListType{ElemType: types.StringType},
	"max_documents":   types.Int64Type,
	"document_count":  types.Int64Type,
	"approver_groups": types.ListType{ElemType: types.StringType},
}

func (d *callerDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_caller"
}

func (d *callerDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	list := func(desc string) schema.ListAttribute {
		return schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Who the provider is logged in as, and what it may write: for a bot writer, its `policy_writer` grant. " +
			"Useful in checks and preconditions, so a run explains a refusal before it sends a write.",
		Attributes: map[string]schema.Attribute{
			"principal":      schema.StringAttribute{Computed: true, MarkdownDescription: "A person's name, or `bot:<name>`."},
			"admin":          schema.BoolAttribute{Computed: true, MarkdownDescription: "True for a person with `limen:admin`."},
			"policy_version": schema.Int64Attribute{Computed: true, MarkdownDescription: "The active policy version."},
			"server_version": schema.StringAttribute{Computed: true, MarkdownDescription: "The control plane's version."},
			"expires_at":     schema.StringAttribute{Computed: true, MarkdownDescription: "When the provider's credentials expire (RFC 3339)."},
			"grant": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The bot's grant; null for a person.",
				Attributes: map[string]schema.Attribute{
					"name":            schema.StringAttribute{Computed: true, MarkdownDescription: "The `policy_writer` document's name."},
					"namespace":       schema.StringAttribute{Computed: true, MarkdownDescription: "The one namespace it writes in."},
					"name_patterns":   list("Patterns every name it writes must match, such as `payments-*`."),
					"kinds":           list("The kinds it may write."),
					"profiles":        list("The profiles its resources may complete."),
					"max_documents":   schema.Int64Attribute{Computed: true, MarkdownDescription: "The most documents it may own."},
					"document_count":  schema.Int64Attribute{Computed: true, MarkdownDescription: "How many it owns now."},
					"approver_groups": list("IdP groups whose members may approve its held changes, besides admins."),
				},
			},
		},
	}
}

func strList(xs []string) types.List {
	vs := make([]attr.Value, len(xs))
	for i, x := range xs {
		vs[i] = types.StringValue(x)
	}
	return types.ListValueMust(types.StringType, vs)
}

func (d *callerDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	c := d.online(&resp.Diagnostics, "limen_caller")
	if c == nil {
		return
	}
	out, err := c.Manage.DescribeCaller(ctx, &managev1.DescribeCallerRequest{})
	if err != nil {
		addError(&resp.Diagnostics, "Asking Limen who the provider is", err)
		return
	}
	m := callerModel{
		Principal:     types.StringValue(out.GetPrincipal()),
		Admin:         types.BoolValue(out.GetAdmin()),
		PolicyVersion: types.Int64Value(out.GetPolicyVersion()),
		ServerVersion: types.StringValue(c.ServerVersion()),
		ExpiresAt:     unixTime(c.ExpiresAt().Unix()),
		Grant:         types.ObjectNull(grantType),
	}
	if g := out.GetGrant(); g != nil {
		kinds := make([]string, 0, len(g.GetKinds()))
		for _, k := range g.GetKinds() {
			kinds = append(kinds, client.KindName(k))
		}
		var diags diag.Diagnostics
		m.Grant, diags = types.ObjectValue(grantType, map[string]attr.Value{
			"name":            types.StringValue(g.GetName()),
			"namespace":       types.StringValue(g.GetNamespace()),
			"name_patterns":   strList(g.GetNamePatterns()),
			"kinds":           strList(kinds),
			"profiles":        strList(g.GetProfiles()),
			"max_documents":   types.Int64Value(int64(g.GetMaxDocuments())),
			"document_count":  types.Int64Value(int64(g.GetDocumentCount())),
			"approver_groups": strList(g.GetApproverGroups()),
		})
		resp.Diagnostics.Append(diags...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---- The spec's schema: limen_schema -------------------------------------------------

type schemaDataSource struct{ base }

func newSchemaDataSource() datasource.DataSource { return &schemaDataSource{} }

type schemaModel struct {
	Kind          types.String `tfsdk:"kind"`
	JSONSchema    types.String `tfsdk:"json_schema"`
	ServerVersion types.String `tfsdk:"server_version"`
}

func (d *schemaDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_schema"
}

func (d *schemaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The JSON Schema (draft 2020-12) of one kind's `spec`, generated by the control plane from the same types it parses with.",
		Attributes: map[string]schema.Attribute{
			"kind": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The kind: one of `" + joinKinds() + "`.",
				Validators:          []validator.String{stringvalidator.OneOf(client.KindNames...)},
			},
			"json_schema":    schema.StringAttribute{Computed: true, MarkdownDescription: "The schema, as JSON."},
			"server_version": schema.StringAttribute{Computed: true, MarkdownDescription: "The server version it came from."},
		},
	}
}

func (d *schemaDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m schemaModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c := d.online(&resp.Diagnostics, "limen_schema")
	if c == nil {
		return
	}
	kind, err := client.ParseKind(m.Kind.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unknown kind", err.Error())
		return
	}
	out, err := c.Manage.GetSchema(ctx, &managev1.GetSchemaRequest{Kind: kind})
	if err != nil {
		addError(&resp.Diagnostics, "Reading the "+m.Kind.ValueString()+" schema", err)
		return
	}
	m.JSONSchema = types.StringValue(out.GetJsonSchema())
	m.ServerVersion = types.StringValue(out.GetServerVersion())
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
