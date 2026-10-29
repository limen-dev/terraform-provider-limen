package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
	"github.com/limen-dev/terraform-provider-limen/internal/specjson"
)

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9._-]{1,128}$`)
	labelKeyRE = regexp.MustCompile(`^[a-z0-9._-]{1,63}$`)
)

// privateSpecKey holds the spec_json Limen returned the last time the state's
// spec was written or confirmed. While Limen still returns those bytes, the
// state keeps the configuration's own spelling of the spec, so defaults
// Limen leaves out don't show as a diff.
const privateSpecKey = "spec_json"

type documentResource struct {
	k    kindInfo
	data *providerData
}

func newDocumentResource(k kindInfo) func() resource.Resource {
	return func() resource.Resource { return &documentResource{k: k} }
}

var (
	_ resource.ResourceWithConfigure      = (*documentResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*documentResource)(nil)
	_ resource.ResourceWithImportState    = (*documentResource)(nil)
	_ resource.ResourceWithValidateConfig = (*documentResource)(nil)
)

// documentModel is the state of every document resource. `profile`, which
// only resources have, is read and written separately.
type documentModel struct {
	ID                types.String  `tfsdk:"id"`
	Name              types.String  `tfsdk:"name"`
	Description       types.String  `tfsdk:"description"`
	Labels            types.Map     `tfsdk:"labels"`
	Spec              types.Dynamic `tfsdk:"spec"`
	Source            types.String  `tfsdk:"source"`
	Namespace         types.String  `tfsdk:"namespace"`
	Channel           types.String  `tfsdk:"channel"`
	EffectiveSpecJSON types.String  `tfsdk:"effective_spec_json"`
	EffectiveLabels   types.Map     `tfsdk:"effective_labels"`
	Etag              types.String  `tfsdk:"etag"`
	PolicyVersion     types.Int64   `tfsdk:"policy_version"`
	CreatedAt         types.String  `tfsdk:"created_at"`
	UpdatedAt         types.String  `tfsdk:"updated_at"`
	UpdatedBy         types.String  `tfsdk:"updated_by"`
}

func (r *documentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + r.k.name
}

func (r *documentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The document's name.",
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		},
		"name": schema.StringAttribute{
			Required: true,
			MarkdownDescription: "Unique per kind across the control plane, and immutable: changing it replaces the document. " +
				"1 to 128 characters of `a-z`, `0-9`, `.`, `_`, and `-`. A bot writer's names must match its grant's name patterns.",
			PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			Validators:    []validator.String{stringvalidator.RegexMatches(nameRE, "must be 1 to 128 characters of a-z, 0-9, '.', '_', and '-'")},
		},
		"description": schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: "Free text shown in the console and the CLI.",
		},
		"labels": schema.MapAttribute{
			Optional:    true,
			ElementType: types.StringType,
			MarkdownDescription: "Labels that roles select on. Keys are lowercase `a-z`, `0-9`, `.`, `_`, `-`. " +
				"For a resource from a profile, only the keys and values the profile allows; its fixed labels are in `effective_labels`.",
			Validators: []validator.Map{
				mapvalidator.KeysAre(stringvalidator.RegexMatches(labelKeyRE, "label keys are 1 to 63 characters of a-z, 0-9, '.', '_', and '-'")),
				mapvalidator.ValueStringsAre(stringvalidator.LengthAtMost(63)),
			},
		},
		"spec": schema.DynamicAttribute{
			Required:            true,
			MarkdownDescription: r.k.specDoc + " Limen parses it strictly: unknown fields are refused. `null` attributes are left out.",
		},
		"source": schema.StringAttribute{
			Optional: true,
			Computed: true,
			MarkdownDescription: "The owner's source. Defaults to the provider's `default_source`. Changing it transfers the document " +
				"(for example after `terraform state mv` into another state); a writer may move documents only between its own sources.",
			Validators: []validator.String{stringvalidator.LengthBetween(1, 200)},
		},
		"namespace": schema.StringAttribute{
			Optional: true,
			Computed: true,
			MarkdownDescription: "The owner's namespace, the permission scope. Defaults to the provider's `default_namespace`, " +
				"else the bot's grant's namespace. Moving a document to another namespace needs a person with `limen:admin`.",
		},
		"channel": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "How the owner writes the document: always `api` for documents Terraform manages.",
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		},
		"effective_spec_json": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "What enforcement uses, as JSON: for a resource from a profile, the profile's template merged with `spec`; " +
				"otherwise the same as `spec`. Use `jsondecode()` to read it.",
		},
		"effective_labels": schema.MapAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "What roles select on: `labels` plus, for a resource from a profile, the profile's fixed labels.",
		},
		"etag": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Opaque; changes whenever the document or its owner changes. Every write sends it back, so Limen refuses to overwrite a change Terraform hasn't seen.",
		},
		"policy_version": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "The policy version this revision first appeared in.",
		},
		"created_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "When the document was created (RFC 3339).",
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		},
		"updated_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "When this revision was written (RFC 3339).",
		},
		"updated_by": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Who wrote this revision: a person, or `bot:<name>`.",
		},
	}
	if r.k.profile {
		attrs["profile"] = schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "The resource profile this resource completes. Bot writers must name one of their grant's profiles; " +
				"a resource without a profile is written in full, which only people with `limen:admin` may do.",
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: r.k.doc + "\n\n" + resourceFooter,
		Attributes:          attrs,
	}
}

const resourceFooter = "Every write is checked against the document's etag and owner, so Terraform never overwrites a change it hasn't seen, " +
	"and never touches a document another channel (Git, the console) or another source owns. " +
	"During plan the provider dry-runs the change (`plan_checks`) and shows who gains or loses access."

func (r *documentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("%T", req.ProviderData))
		return
	}
	r.data = data
}

func (r *documentResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var spec types.Dynamic
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("spec"), &spec)...)
	if resp.Diagnostics.HasError() || spec.IsNull() || spec.IsUnknown() || spec.IsUnderlyingValueUnknown() {
		return
	}
	shape := valueShape(spec.UnderlyingValue())
	want := "object"
	if r.k.kind == managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING {
		want = "list"
	}
	if shape != want && shape != "" {
		resp.Diagnostics.AddAttributeError(path.Root("spec"), "Wrong spec shape",
			fmt.Sprintf("A %s's spec is %s %s, as in YAML; this is %s %s.", r.k.what, article(want), want, article(shape), shape))
	}
}

func valueShape(v attr.Value) string {
	switch v.(type) {
	case basetypes.ObjectValue, basetypes.MapValue:
		return "object"
	case basetypes.ListValue, basetypes.TupleValue, basetypes.SetValue:
		return "list"
	case basetypes.StringValue:
		return "string"
	case basetypes.NumberValue, basetypes.Int64Value, basetypes.Float64Value:
		return "number"
	case basetypes.BoolValue:
		return "bool"
	}
	return ""
}

func article(s string) string {
	if strings.ContainsAny(s[:1], "aeiou") {
		return "an"
	}
	return "a"
}

// ---- Reading and writing the model -------------------------------------------

// withProfile is the state of limen_resource, the one kind with a profile.
type withProfile struct {
	documentModel
	Profile types.String `tfsdk:"profile"`
}

// get reads a plan, state, or configuration into a model and the profile
// (null for kinds without one).
func (r *documentResource) get(ctx context.Context, getter func(context.Context, any) diag.Diagnostics) (documentModel, types.String, diag.Diagnostics) {
	if r.k.profile {
		var m withProfile
		diags := getter(ctx, &m)
		return m.documentModel, m.Profile, diags
	}
	var m documentModel
	diags := getter(ctx, &m)
	return m, types.StringNull(), diags
}

func (r *documentResource) set(ctx context.Context, setter func(context.Context, any) diag.Diagnostics, m documentModel, profile types.String) diag.Diagnostics {
	if r.k.profile {
		return setter(ctx, &withProfile{documentModel: m, Profile: profile})
	}
	return setter(ctx, &m)
}

func (r *documentResource) toDocument(ctx context.Context, m *documentModel, profile types.String) (*managev1.Document, error) {
	spec, err := specjson.FromValue(ctx, m.Spec)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for k, v := range m.Labels.Elements() {
		if s, ok := v.(types.String); ok {
			labels[k] = s.ValueString()
		}
	}
	return &managev1.Document{
		Kind:        r.k.kind,
		Name:        m.Name.ValueString(),
		Description: m.Description.ValueString(),
		Labels:      labels,
		SpecJson:    string(spec),
		Profile:     profile.ValueString(),
	}, nil
}

func unixTime(s int64) types.String {
	if s == 0 {
		return types.StringNull()
	}
	return types.StringValue(time.Unix(s, 0).UTC().Format(time.RFC3339))
}

func stringMap(m map[string]string) types.Map {
	elems := make(map[string]attr.Value, len(m))
	for k, v := range m {
		elems[k] = types.StringValue(v)
	}
	return types.MapValueMust(types.StringType, elems)
}

// setComputed copies what Limen computes from doc into m.
func setComputed(m *documentModel, doc *managev1.Document) {
	m.ID = types.StringValue(doc.GetName())
	m.Source = types.StringValue(doc.GetOwnership().GetSource())
	m.Namespace = types.StringValue(doc.GetOwnership().GetNamespace())
	m.Channel = types.StringValue(client.ChannelName(doc.GetOwnership().GetChannel()))
	m.EffectiveSpecJSON = types.StringValue(doc.GetEffectiveSpecJson())
	m.EffectiveLabels = stringMap(doc.GetEffectiveLabels())
	m.Etag = types.StringValue(doc.GetEtag())
	m.PolicyVersion = types.Int64Value(doc.GetPolicyVersion())
	m.CreatedAt = unixTime(doc.GetCreateTimeUnix())
	m.UpdatedAt = unixTime(doc.GetUpdateTimeUnix())
	m.UpdatedBy = types.StringValue(doc.GetUpdatedBy())
}

// setContent copies the configurable fields from doc into m, keeping m's own
// spelling where Limen's answer means the same thing.
func setContent(ctx context.Context, m *documentModel, doc *managev1.Document, lastSpec []byte) error {
	m.Name = types.StringValue(doc.GetName())
	if doc.GetDescription() != "" || !m.Description.IsNull() {
		m.Description = types.StringValue(doc.GetDescription())
	}
	if len(doc.GetLabels()) > 0 || !m.Labels.IsNull() {
		m.Labels = stringMap(doc.GetLabels())
	}
	keep := false
	if !m.Spec.IsNull() && !m.Spec.IsUnknown() {
		if lastSpec != nil && string(lastSpec) == doc.GetSpecJson() {
			keep = true
		} else if mine, err := specjson.FromValue(ctx, m.Spec); err == nil && specjson.Equal(mine, []byte(doc.GetSpecJson())) {
			keep = true
		}
	}
	if !keep {
		v, err := specjson.ToValue([]byte(doc.GetSpecJson()))
		if err != nil {
			return fmt.Errorf("the spec Limen returned isn't JSON: %w", err)
		}
		m.Spec = v
	}
	return nil
}

func ownership(m *documentModel) *managev1.Ownership {
	return &managev1.Ownership{Source: m.Source.ValueString(), Namespace: m.Namespace.ValueString()}
}

func (r *documentResource) address(name string) string {
	return fmt.Sprintf("limen_%s.%s", r.k.name, name)
}

func (r *documentResource) ref(name string) string { return r.k.name + "/" + name }

func (r *documentResource) online(diags *diag.Diagnostics, doing string) bool {
	if r.data == nil {
		diags.AddError("Provider not configured", "The Limen provider wasn't configured before "+doing+".")
		return false
	}
	if r.data.client == nil {
		diags.AddError("Limen provider is offline",
			"Can't "+doing+" with offline = true. Offline mode is for plans in pull requests; apply from a run that can log in to Limen.")
		return false
	}
	return true
}

// finish turns a write's result into the document as written, waiting for
// approval when Limen held the change and the provider was asked to.
func (r *documentResource) finish(ctx context.Context, res *managev1.WriteResult, name string) (*managev1.Document, error) {
	c := r.data.client
	if res.GetOutcome() == managev1.WriteOutcome_WRITE_OUTCOME_PENDING_APPROVAL {
		if err := r.awaitApproval(ctx, res.GetPendingChange(), name); err != nil {
			return nil, err
		}
		return c.GetDocument(ctx, r.k.kind, name)
	}
	for _, d := range res.GetDocuments() {
		if d.GetKind() == r.k.kind && d.GetName() == name {
			return d, nil
		}
	}
	return c.GetDocument(ctx, r.k.kind, name)
}

// awaitApproval handles a held change: it replaces this configuration's
// earlier pending changes to the document, then waits for a decision when
// approval_timeout allows, or returns an *client.ErrPending.
func (r *documentResource) awaitApproval(ctx context.Context, ch *managev1.PolicyChange, name string) error {
	c := r.data.client
	replaced, err := c.CancelSuperseded(ctx, ch, r.k.kind, name)
	if err != nil {
		// The new change stands; failing to tidy up older ones is not fatal.
		tflog.Warn(ctx, "couldn't cancel earlier pending changes", map[string]any{"error": err.Error()})
	}
	if r.data.approvalTimeout <= 0 {
		return &client.ErrPending{Change: ch, Replaced: replaced}
	}
	_, err = c.WaitForChange(ctx, ch, r.data.approvalTimeout, pollInterval(r.data.approvalTimeout))
	if p, ok := client.AsPending(err); ok {
		p.Replaced = replaced
	}
	return err
}

// ---- Plan ---------------------------------------------------------------------

func (r *documentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.data == nil {
		return
	}
	if req.Plan.Raw.IsNull() {
		r.planDestroy(ctx, req, resp)
		return
	}
	plan, planProfile, d := r.get(ctx, req.Plan.Get)
	resp.Diagnostics.Append(d...)
	config, _, d := r.get(ctx, req.Config.Get)
	resp.Diagnostics.Append(d...)
	var state *documentModel
	stateProfile := types.StringNull()
	if !req.State.Raw.IsNull() {
		var st documentModel
		st, stateProfile, d = r.get(ctx, req.State.Get)
		resp.Diagnostics.Append(d...)
		state = &st
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// The owner: what the configuration says, else the provider's defaults,
	// else (for an existing document) where it is now.
	if config.Source.IsNull() {
		switch {
		case r.data.source != "":
			plan.Source = types.StringValue(r.data.source)
		case state != nil:
			plan.Source = state.Source
		default:
			resp.Diagnostics.AddAttributeError(path.Root("source"), "No source for the new document",
				"Set the provider's default_source (or LIMEN_SOURCE), such as terraform/payments-prod: one source per Terraform state. "+
					"It records which configuration owns the document, so two configurations can't overwrite each other's documents by accident.")
			return
		}
	}
	if config.Namespace.IsNull() {
		ns, err := r.data.defaultNamespace(ctx)
		switch {
		case err != nil:
			addError(&resp.Diagnostics, "Asking Limen for the writer's namespace", err)
			return
		case ns != "":
			plan.Namespace = types.StringValue(ns)
		case state != nil:
			plan.Namespace = state.Namespace
		default:
			plan.Namespace = types.StringUnknown()
		}
	}

	contentChanged, ownerChanged := true, true
	if state != nil {
		contentChanged = !plan.Description.Equal(state.Description) || !plan.Labels.Equal(state.Labels) ||
			!planProfile.Equal(stateProfile) || !sameSpec(ctx, plan.Spec, state.Spec)
		ownerChanged = !plan.Source.Equal(state.Source) || !plan.Namespace.Equal(state.Namespace)
	}
	if state == nil {
		plan.ID = plan.Name
		plan.Channel = types.StringUnknown()
		plan.CreatedAt = types.StringUnknown()
	}
	if ownerChanged {
		plan.Channel = types.StringUnknown()
	}
	if contentChanged || ownerChanged {
		plan.Etag = types.StringUnknown()
		plan.PolicyVersion = types.Int64Unknown()
		plan.UpdatedAt = types.StringUnknown()
		plan.UpdatedBy = types.StringUnknown()
	} else {
		plan.Etag, plan.PolicyVersion, plan.UpdatedAt, plan.UpdatedBy = state.Etag, state.PolicyVersion, state.UpdatedAt, state.UpdatedBy
	}
	if contentChanged {
		plan.EffectiveSpecJSON = types.StringUnknown()
		plan.EffectiveLabels = types.MapUnknown(types.StringType)
	} else {
		plan.EffectiveSpecJSON, plan.EffectiveLabels = state.EffectiveSpecJSON, state.EffectiveLabels
	}
	resp.Diagnostics.Append(r.set(ctx, resp.Plan.Set, plan, planProfile)...)
	if state != nil && ownerChanged && state.Channel.ValueString() != "" && state.Channel.ValueString() != "api" {
		resp.Diagnostics.AddWarning("Taking over a document: "+r.ref(plan.Name.ValueString()),
			fmt.Sprintf("%s is owned by channel %q, source %q, namespace %q. Apply moves it to this configuration (source %q, namespace %q), "+
				"and its current owner can no longer change it. Only a person with limen:admin may do this. "+
				"To leave it where it is, set source and namespace on the resource to its current owner's and change it there instead.",
				r.ref(plan.Name.ValueString()), state.Channel.ValueString(), state.Source.ValueString(), state.Namespace.ValueString(),
				plan.Source.ValueString(), plan.Namespace.ValueString()))
	}
	if resp.Diagnostics.HasError() || !contentChanged && !ownerChanged && (state == nil || plan.Name.Equal(state.Name)) {
		return
	}
	if state != nil && !plan.Name.Equal(state.Name) {
		// A new name replaces the document. Terraform plans the replacement
		// again as a create, which is dry-run then; nothing to check here.
		if r.k.kind == managev1.DocumentKind_DOCUMENT_KIND_RESOURCE {
			resp.Diagnostics.AddWarning("Replacing a resource ends its sessions: "+r.ref(state.Name.ValueString()),
				"Names never change in Limen, so renaming deletes "+state.Name.ValueString()+" and creates "+plan.Name.ValueString()+
					". Deleting it ends its open sessions and active access grants (ADR-0042 §7).")
		}
		return
	}
	if r.data.client == nil || !r.data.planChecks {
		return
	}
	if !specjson.IsFullyKnown(plan.Spec) || plan.Labels.IsUnknown() || plan.Description.IsUnknown() || planProfile.IsUnknown() ||
		plan.Source.IsUnknown() || plan.Namespace.IsUnknown() {
		// Values from resources not yet created: Limen checks at apply.
		return
	}
	r.dryRun(ctx, &plan, planProfile, state, contentChanged, ownerChanged, &resp.Diagnostics)
}

func sameSpec(ctx context.Context, a, b types.Dynamic) bool {
	if !specjson.IsFullyKnown(a) || !specjson.IsFullyKnown(b) {
		return a.Equal(b)
	}
	ja, errA := specjson.FromValue(ctx, a)
	jb, errB := specjson.FromValue(ctx, b)
	if errA != nil || errB != nil {
		return false
	}
	// Same data, but a change of spelling (types) is still a change in
	// Terraform's eyes; it is applied and Limen reports it unchanged.
	return string(ja) == string(jb) && a.Equal(b)
}

// dryRun sends the planned write with validate_only, so the plan shows what
// Limen would say: refusals as errors, and who gains or loses access.
func (r *documentResource) dryRun(ctx context.Context, plan *documentModel, profile types.String, state *documentModel, contentChanged, ownerChanged bool, diags *diag.Diagnostics) {
	c := r.data.client
	name := plan.Name.ValueString()
	addr := r.ref(name)
	doc, err := r.toDocument(ctx, plan, profile)
	if err != nil {
		diags.AddAttributeError(path.Root("spec"), "Invalid spec", err.Error())
		return
	}
	var results []*managev1.WriteResult
	note := func(err error, doing string) bool {
		if err == nil {
			return true
		}
		if client.Is(err, client.ReasonApprovalRequired) {
			diags.AddWarning("Change will need approval: "+addr,
				"Limen will hold this change for a person to approve, because it widens standing access. "+
					approvalHint(r.data.approvalTimeout))
			return false
		}
		addError(diags, doing, err)
		return false
	}
	if state == nil {
		resp, err := c.Manage.CreateDocument(ctx, &managev1.CreateDocumentRequest{Document: doc, Ownership: ownership(plan), Options: r.data.writeOptions(true)})
		if client.IsAlreadyExists(err) {
			r.planAdoption(ctx, plan, diags)
			return
		}
		if !note(err, "Checking the new "+r.k.what+" "+name+" with Limen") {
			return
		}
		results = append(results, resp.GetResult())
	} else {
		if ownerChanged {
			resp, err := c.Manage.TransferDocument(ctx, &managev1.TransferDocumentRequest{
				Kind: r.k.kind, Name: name, Etag: state.Etag.ValueString(), Ownership: ownership(plan), Options: r.data.writeOptions(true),
			})
			if !note(err, fmt.Sprintf("Checking the move of %s to source %q, namespace %q", r.ref(name), plan.Source.ValueString(), plan.Namespace.ValueString())) {
				return
			}
			results = append(results, resp.GetResult())
		}
		if contentChanged && !ownerChanged {
			resp, err := c.Manage.UpdateDocument(ctx, &managev1.UpdateDocumentRequest{Document: doc, Etag: state.Etag.ValueString(), Ownership: ownership(state), Options: r.data.writeOptions(true)})
			if !note(err, "Checking the change to "+r.ref(name)+" with Limen") {
				return
			}
			results = append(results, resp.GetResult())
		}
	}
	for _, res := range results {
		addImpact(diags, addr, res.GetImpact())
	}
}

// pollInterval asks about a held change about 30 times over the wait,
// between once a second and every 10 seconds.
func pollInterval(wait time.Duration) time.Duration {
	return min(max(wait/30, time.Second), 10*time.Second)
}

func approvalHint(timeout time.Duration) string {
	if timeout > 0 {
		return fmt.Sprintf("Apply will wait up to %s (approval_timeout) for the decision.", timeout)
	}
	return "Apply will stop and name the change; after a person approves it, apply again. Set approval_timeout to wait instead."
}

// planAdoption explains, at plan time, a create whose name is already taken.
func (r *documentResource) planAdoption(ctx context.Context, plan *documentModel, diags *diag.Diagnostics) {
	name := plan.Name.ValueString()
	cur, err := r.data.client.GetDocument(ctx, r.k.kind, name)
	if err != nil || cur == nil {
		diags.AddAttributeError(path.Root("name"), "Name already taken",
			fmt.Sprintf("Limen already has a %s named %q, which this caller can't read. Names are unique per kind across the control plane; choose another.", r.k.what, name))
		return
	}
	if mine(cur, plan) {
		diags.AddWarning("Document already exists: "+r.ref(name),
			fmt.Sprintf("%s already exists in this source (for example, a held change was approved after an earlier apply). "+
				"Apply takes it over if its content matches this configuration; otherwise import it.", r.ref(name)))
		return
	}
	o := cur.GetOwnership()
	diags.AddAttributeError(path.Root("name"), "Name already taken",
		fmt.Sprintf("%s already exists, owned by channel %q, source %q, namespace %q. Choose another name, or import it if this configuration should own it: "+
			"terraform import %s %s", r.ref(name), client.ChannelName(o.GetChannel()), o.GetSource(), o.GetNamespace(), r.address("<name>"), name))
}

func mine(doc *managev1.Document, m *documentModel) bool {
	o := doc.GetOwnership()
	return o.GetChannel() == managev1.Channel_CHANNEL_API && o.GetSource() == m.Source.ValueString() && o.GetNamespace() == m.Namespace.ValueString()
}

func (r *documentResource) planDestroy(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.data.client == nil || !r.data.planChecks {
		return
	}
	state, _, d := r.get(ctx, req.State.Get)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	res, err := r.data.client.Manage.DeleteDocument(ctx, &managev1.DeleteDocumentRequest{
		Kind: r.k.kind, Name: name, Etag: state.Etag.ValueString(), AllowMissing: true, Ownership: ownership(&state), Options: r.data.writeOptions(true),
	})
	if err != nil {
		if client.Is(err, client.ReasonApprovalRequired) {
			resp.Diagnostics.AddWarning("Delete will need approval: "+r.ref(name), approvalHint(r.data.approvalTimeout))
			return
		}
		addError(&resp.Diagnostics, "Checking the delete of "+r.ref(name)+" with Limen", err)
		return
	}
	addImpact(&resp.Diagnostics, r.ref(name), res.GetResult().GetImpact())
	if r.k.kind == managev1.DocumentKind_DOCUMENT_KIND_RESOURCE {
		resp.Diagnostics.AddWarning("Deleting a resource ends its sessions: "+r.ref(name),
			"When "+name+" is deleted, Limen ends its open sessions and active access grants (ADR-0042 §7).")
	}
}

// ---- Apply --------------------------------------------------------------------

func (r *documentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.online(&resp.Diagnostics, "create a "+r.k.what) {
		return
	}
	plan, profile, d := r.get(ctx, req.Plan.Get)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Namespace.IsUnknown() {
		ns, err := r.data.defaultNamespace(ctx)
		if err != nil {
			addError(&resp.Diagnostics, "Asking Limen for the writer's namespace", err)
			return
		}
		plan.Namespace = types.StringValue(ns)
	}
	name := plan.Name.ValueString()
	doc, err := r.toDocument(ctx, &plan, profile)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("spec"), "Invalid spec", err.Error())
		return
	}
	c := r.data.client
	res, err := c.Manage.CreateDocument(ctx, &managev1.CreateDocumentRequest{Document: doc, Ownership: ownership(&plan), Options: r.data.writeOptions(false)})
	var written *managev1.Document
	switch {
	case client.IsAlreadyExists(err):
		written, err = r.adopt(ctx, &plan, doc)
	case err == nil:
		written, err = r.finish(ctx, res.GetResult(), name)
	}
	if err == nil && written == nil {
		err = fmt.Errorf("%s was reported written but can't be read back; run terraform plan to see it", r.ref(name))
	}
	if err != nil {
		addError(&resp.Diagnostics, "Creating "+r.ref(name), err)
		return
	}
	setComputed(&plan, written)
	resp.Diagnostics.Append(r.set(ctx, resp.State.Set, plan, profile)...)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateSpecKey, jsonString(written.GetSpecJson()))...)
}

// adopt takes over a document that already exists in this configuration's
// own source with exactly the planned content: what is left after a held
// change was approved, or after an apply whose state wasn't saved. Anything
// else is refused.
func (r *documentResource) adopt(ctx context.Context, plan *documentModel, doc *managev1.Document) (*managev1.Document, error) {
	c := r.data.client
	name := plan.Name.ValueString()
	cur, err := c.GetDocument(ctx, r.k.kind, name)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%s was reported as existing but can't be read; apply again", r.ref(name))
	}
	if !mine(cur, plan) {
		o := cur.GetOwnership()
		return nil, fmt.Errorf("%s already exists, owned by channel %q, source %q, namespace %q. Names are unique per kind; choose another, or import it",
			r.ref(name), client.ChannelName(o.GetChannel()), o.GetSource(), o.GetNamespace())
	}
	check, err := c.Manage.UpdateDocument(ctx, &managev1.UpdateDocumentRequest{Document: doc, Etag: cur.GetEtag(), Ownership: ownership(plan), Options: &managev1.WriteOptions{ValidateOnly: true}})
	if err != nil {
		return nil, err
	}
	if check.GetResult().GetOutcome() != managev1.WriteOutcome_WRITE_OUTCOME_UNCHANGED {
		return nil, fmt.Errorf("%s already exists in this source with different content, so it isn't taken over. "+
			"If this configuration should own it, import it: terraform import %s %s", r.ref(name), r.address("<name>"), name)
	}
	return cur, nil
}

func jsonString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

func lastSpec(ctx context.Context, get func(context.Context, string) ([]byte, diag.Diagnostics)) []byte {
	raw, diags := get(ctx, privateSpecKey)
	if diags.HasError() || len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return []byte(s)
}

func (r *documentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if r.data == nil || r.data.client == nil {
		// Offline: the state is what it was.
		return
	}
	state, profile, d := r.get(ctx, req.State.Get)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if name == "" {
		name = state.ID.ValueString()
	}
	doc, err := r.data.client.GetDocument(ctx, r.k.kind, name)
	if err != nil {
		addError(&resp.Diagnostics, "Reading "+r.ref(name), err)
		return
	}
	if doc == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	if err := setContent(ctx, &state, doc, lastSpec(ctx, req.Private.GetKey)); err != nil {
		resp.Diagnostics.AddError("Reading "+r.ref(name), err.Error())
		return
	}
	setComputed(&state, doc)
	if r.k.profile && (doc.GetProfile() != "" || !profile.IsNull()) {
		profile = types.StringValue(doc.GetProfile())
	}
	resp.Diagnostics.Append(r.set(ctx, resp.State.Set, state, profile)...)
	// Remember Limen's spelling only while the state's spec still means the
	// same thing as it.
	if mineSpec, err := specjson.FromValue(ctx, state.Spec); err == nil && specjson.Equal(mineSpec, []byte(doc.GetSpecJson())) {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateSpecKey, jsonString(doc.GetSpecJson()))...)
	}
}

func (r *documentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.online(&resp.Diagnostics, "change a "+r.k.what) {
		return
	}
	plan, planProfile, d := r.get(ctx, req.Plan.Get)
	resp.Diagnostics.Append(d...)
	state, stateProfile, d := r.get(ctx, req.State.Get)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	c := r.data.client
	name := plan.Name.ValueString()
	etag := state.Etag.ValueString()
	var written *managev1.Document

	if !plan.Source.Equal(state.Source) || !plan.Namespace.Equal(state.Namespace) {
		res, err := c.Manage.TransferDocument(ctx, &managev1.TransferDocumentRequest{
			Kind: r.k.kind, Name: name, Etag: etag, Ownership: ownership(&plan), Options: r.data.writeOptions(false),
		})
		if err == nil {
			written, err = r.finish(ctx, res.GetResult(), name)
		}
		if err != nil {
			addError(&resp.Diagnostics, fmt.Sprintf("Moving %s to source %q, namespace %q", r.ref(name), plan.Source.ValueString(), plan.Namespace.ValueString()), err)
			return
		}
		etag = written.GetEtag()
	}
	contentChanged := !plan.Description.Equal(state.Description) || !plan.Labels.Equal(state.Labels) ||
		!planProfile.Equal(stateProfile) || !plan.Spec.Equal(state.Spec)
	if contentChanged || written == nil {
		doc, err := r.toDocument(ctx, &plan, planProfile)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("spec"), "Invalid spec", err.Error())
			return
		}
		// The owner is plan's: a transfer above already moved the document there.
		res, err := c.Manage.UpdateDocument(ctx, &managev1.UpdateDocumentRequest{Document: doc, Etag: etag, Ownership: ownership(&plan), Options: r.data.writeOptions(false)})
		if err == nil {
			written, err = r.finish(ctx, res.GetResult(), name)
		}
		if err == nil && written == nil {
			err = fmt.Errorf("%s was deleted during the update", r.ref(name))
		}
		if err != nil {
			addError(&resp.Diagnostics, "Changing "+r.ref(name), err)
			return
		}
	}
	setComputed(&plan, written)
	resp.Diagnostics.Append(r.set(ctx, resp.State.Set, plan, planProfile)...)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateSpecKey, jsonString(written.GetSpecJson()))...)
}

func (r *documentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.online(&resp.Diagnostics, "delete a "+r.k.what) {
		return
	}
	state, _, d := r.get(ctx, req.State.Get)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	res, err := r.data.client.Manage.DeleteDocument(ctx, &managev1.DeleteDocumentRequest{
		Kind: r.k.kind, Name: name, Etag: state.Etag.ValueString(), AllowMissing: true, Ownership: ownership(&state), Options: r.data.writeOptions(false),
	})
	if err == nil && res.GetResult().GetOutcome() == managev1.WriteOutcome_WRITE_OUTCOME_PENDING_APPROVAL {
		err = r.awaitApproval(ctx, res.GetResult().GetPendingChange(), name)
	}
	if client.Is(err, client.ReasonOwnedElsewhere) {
		resp.Diagnostics.AddError("Document is managed somewhere else",
			fmt.Sprintf("Deleting %s: it was moved to another owner since Terraform created it, so this configuration may no longer delete it.\n\n%s\n\n"+
				"To stop managing it here without deleting it, remove it from the state: terraform state rm (or a removed block).",
				r.ref(name), err.Error()))
		return
	}
	if err != nil {
		addError(&resp.Diagnostics, "Deleting "+r.ref(name), err)
	}
}

func (r *documentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name := req.ID
	if kind, rest, ok := strings.Cut(req.ID, "/"); ok {
		if kind != r.k.name {
			resp.Diagnostics.AddError("Wrong kind", fmt.Sprintf("%q names a %s; this is limen_%s. Import it by name: %s", req.ID, kind, r.k.name, rest))
			return
		}
		name = rest
	}
	if !nameRE.MatchString(name) {
		resp.Diagnostics.AddError("Invalid name", fmt.Sprintf("%q is not a document name. Import by name, such as payments-orders-db.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
}
