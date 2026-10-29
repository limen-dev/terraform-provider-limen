package fakelimen

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

const domain = "manage.limen.dev"

// Kinds only people may write, through git or the console (ADR-0042 §4).
var peopleOnly = map[managev1.DocumentKind]bool{
	managev1.DocumentKind_DOCUMENT_KIND_BOT:              true,
	managev1.DocumentKind_DOCUMENT_KIND_DISCOVERY:        true,
	managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE: true,
	managev1.DocumentKind_DOCUMENT_KIND_POLICY_WRITER:    true,
}

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9._-]{1,128}$`)
	labelRE = regexp.MustCompile(`^[a-z0-9._-]{1,63}$`)
	targets = []string{"ssh", "postgres", "mysql", "redshift", "redis", "mongodb", "opensearch", "elasticsearch", "kubernetes", "web", "tcp"}
)

func kindName(k managev1.DocumentKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "DOCUMENT_KIND_"))
}

func apiError(code codes.Code, reason, msg string, md map[string]string, retryAfter time.Duration) error {
	st := status.New(code, msg)
	details := []protoadapt.MessageV1{}
	if reason != "" {
		details = append(details, &errdetails.ErrorInfo{Reason: reason, Domain: domain, Metadata: md})
	}
	if retryAfter > 0 {
		details = append(details, &errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)})
	}
	if len(details) > 0 {
		if withDetails, err := st.WithDetails(details...); err == nil {
			st = withDetails
		}
	}
	return st.Err()
}

func invalid(violations ...[2]string) error {
	st := status.New(codes.InvalidArgument, "the document is invalid")
	br := &errdetails.BadRequest{}
	for _, v := range violations {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: v[0], Description: v[1]})
	}
	st, _ = st.WithDetails(&errdetails.ErrorInfo{Reason: "INVALID_DOCUMENT", Domain: domain}, br)
	return st.Err()
}

func outsideGrant(rule, msg string) error {
	return apiError(codes.PermissionDenied, "OUTSIDE_GRANT", msg, map[string]string{"rule": rule}, 0)
}

func cloneDoc(d *managev1.Document) *managev1.Document {
	return proto.Clone(d).(*managev1.Document)
}

func cloneOwnership(o *managev1.Ownership) *managev1.Ownership {
	return proto.Clone(o).(*managev1.Ownership)
}

func newKey() (*ecdsa.PrivateKey, []byte, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return k, nil, err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- Callers ----------------------------------------------------------------

type caller struct {
	principal string
	admin     bool
	writer    *Writer
}

func (s *Server) caller(principal string) (caller, error) {
	c := caller{principal: principal, admin: s.admins[principal]}
	if bot, ok := strings.CutPrefix(principal, "bot:"); ok {
		c.writer = s.writers[bot]
	}
	if !c.admin && c.writer == nil {
		return c, status.Error(codes.PermissionDenied, principal+" may not use the management API")
	}
	return c, nil
}

func (c caller) mayKind(k managev1.DocumentKind) bool {
	for _, x := range c.writer.Kinds {
		if x == k {
			return true
		}
	}
	return false
}

func (c caller) matchesName(name string) bool {
	for _, p := range c.writer.NamePatterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// authorize checks the grant before anything about the document, so a
// writer learns nothing about names outside it.
func (c caller) authorize(kind managev1.DocumentKind, name, namespace string) error {
	if peopleOnly[kind] {
		return outsideGrant("kind", kindName(kind)+" documents are written only by people, through git or the console")
	}
	if c.writer == nil {
		return nil
	}
	if !c.matchesName(name) {
		return outsideGrant("name_pattern", fmt.Sprintf("%q matches none of the writer's name patterns", name))
	}
	if !c.mayKind(kind) {
		return outsideGrant("kind", "the writer may not write "+kindName(kind)+" documents")
	}
	if namespace != c.writer.Namespace {
		return outsideGrant("namespace", fmt.Sprintf("the writer's namespace is %q", c.writer.Namespace))
	}
	return nil
}

// ---- Validation and completion ------------------------------------------------

type profileSpec struct {
	Template map[string]any            `json:"template"`
	Settable map[string]map[string]any `json:"settable"`
	Labels   struct {
		Fixed   map[string]string   `json:"fixed"`
		Allowed map[string][]string `json:"allowed"`
	} `json:"labels"`
}

// complete validates d, normalizes its spec, and computes its effective
// spec and labels.
func (s *Server) complete(d *managev1.Document, _ string) error {
	if !nameRE.MatchString(d.Name) {
		return invalid([2]string{"name", "1 to 128 characters of a-z, 0-9, '.', '_', and '-'"})
	}
	for k, v := range d.Labels {
		if !labelRE.MatchString(k) || len(v) > 63 {
			return invalid([2]string{"labels." + k, "label keys are lowercase a-z, 0-9, '.', '_', '-'; values up to 63 characters"})
		}
	}
	var spec any
	if err := json.Unmarshal([]byte(d.SpecJson), &spec); err != nil {
		return invalid([2]string{"spec", "not JSON: " + err.Error()})
	}
	effective := spec
	effectiveLabels := map[string]string{}
	for k, v := range d.Labels {
		effectiveLabels[k] = v
	}
	if d.Profile != "" {
		if d.Kind != managev1.DocumentKind_DOCUMENT_KIND_RESOURCE {
			return invalid([2]string{"profile", "only resources complete a profile"})
		}
		pd := s.docs[docKey{managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE, d.Profile}]
		if pd == nil {
			return invalid([2]string{"profile", fmt.Sprintf("no resource_profile named %q", d.Profile)})
		}
		var p profileSpec
		if err := json.Unmarshal([]byte(pd.SpecJson), &p); err != nil {
			return status.Error(codes.Internal, "bad profile")
		}
		obj, ok := spec.(map[string]any)
		if !ok {
			return invalid([2]string{"spec", "must be an object"})
		}
		if err := checkSettable(obj, "", p.Settable); err != nil {
			return err
		}
		for k, v := range d.Labels {
			if _, fixed := p.Labels.Fixed[k]; fixed {
				return outsideGrant("label", fmt.Sprintf("label %q is fixed by profile %s", k, d.Profile))
			}
			allowed, ok := p.Labels.Allowed[k]
			if !ok || !contains(allowed, v) {
				return outsideGrant("label", fmt.Sprintf("profile %s doesn't allow %s=%s", d.Profile, k, v))
			}
		}
		for k, v := range p.Labels.Fixed {
			effectiveLabels[k] = v
		}
		effective = merge(p.Template, obj)
	}
	switch d.Kind {
	case managev1.DocumentKind_DOCUMENT_KIND_RESOURCE:
		obj, ok := effective.(map[string]any)
		if !ok {
			return invalid([2]string{"spec", "must be an object"})
		}
		var set []string
		for k := range obj {
			switch {
			case contains(targets, k):
				set = append(set, k)
			case k == "worker_selector":
			default:
				return invalid([2]string{"spec." + k, "unknown field"})
			}
		}
		if len(set) != 1 {
			return invalid([2]string{"spec", "exactly one target must be set: " + strings.Join(targets, ", ")})
		}
	case managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING:
		rows, ok := spec.([]any)
		if !ok {
			return invalid([2]string{"spec", "must be a list of {idp_group, roles}"})
		}
		for i, r := range rows {
			row, ok := r.(map[string]any)
			if _, hasGroup := row["idp_group"].(string); !ok || !hasGroup {
				return invalid([2]string{fmt.Sprintf("spec[%d].idp_group", i), "required"})
			}
		}
	case managev1.DocumentKind_DOCUMENT_KIND_POLICY_TEST:
		obj, ok := spec.(map[string]any)
		if _, hasCases := obj["cases"].([]any); !ok || !hasCases {
			return invalid([2]string{"spec.cases", "required: a list of test cases"})
		}
	default:
		if _, ok := spec.(map[string]any); !ok {
			return invalid([2]string{"spec", "must be an object"})
		}
	}
	d.SpecJson = normalize(spec)
	d.EffectiveSpecJson = normalize(effective)
	d.EffectiveLabels = effectiveLabels
	return nil
}

// checkSettable refuses any leaf the profile doesn't make settable, and
// endpoints outside the settable field's host and port bounds.
func checkSettable(obj map[string]any, prefix string, settable map[string]map[string]any) error {
	for k, v := range obj {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if bounds, ok := settable[p]; ok {
			if err := checkBounds(p, v, bounds); err != nil {
				return err
			}
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			if err := checkSettable(sub, p, settable); err != nil {
				return err
			}
			continue
		}
		return outsideGrant("field", fmt.Sprintf("%s is not settable in this profile", p))
	}
	return nil
}

func checkBounds(field string, v any, bounds map[string]any) error {
	hosts, _ := bounds["hosts"].([]any)
	if len(hosts) == 0 {
		return nil
	}
	addr, _ := v.(string)
	host, port, found := strings.Cut(addr, ":")
	if !found {
		return invalid([2]string{"spec." + field, "must be host:port"})
	}
	okHost := false
	for _, h := range hosts {
		pat, _ := h.(string)
		if pat == host || strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:]) {
			okHost = true
		}
	}
	if !okHost {
		return outsideGrant("field", fmt.Sprintf("%s: host %s is outside the profile's hosts", field, host))
	}
	if ports, _ := bounds["ports"].([]any); len(ports) > 0 {
		okPort := false
		for _, p := range ports {
			if fmt.Sprint(p) == port {
				okPort = true
			}
		}
		if !okPort {
			return outsideGrant("field", fmt.Sprintf("%s: port %s is outside the profile's ports", field, port))
		}
	}
	return nil
}

func merge(base map[string]any, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = merge(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// normalize re-encodes with sorted keys (encoding/json sorts map keys), and
// leaves out nulls and `false`, standing in for Limen leaving out defaults.
func normalize(v any) string {
	b, _ := json.Marshal(dropDefaults(v))
	return string(b)
}

func dropDefaults(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range v {
			if e == nil || e == false {
				continue
			}
			out[k] = dropDefaults(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = dropDefaults(e)
		}
		return out
	}
	return v
}

func sameContent(a, b *managev1.Document) bool {
	return a.Description == b.Description && a.SpecJson == b.SpecJson && a.Profile == b.Profile && equalMaps(a.Labels, b.Labels)
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (s *Server) stamp(d *managev1.Document, by string) {
	now := time.Now().Unix()
	d.Etag = randomHex(8)
	d.PolicyVersion = s.version
	if d.CreateTimeUnix == 0 {
		d.CreateTimeUnix = now
	}
	d.UpdateTimeUnix = now
	d.UpdatedBy = by
}

// ---- Writes -----------------------------------------------------------------------

type requestRecord struct {
	fingerprint [32]byte
	result      *managev1.WriteResult
	err         error
}

type operation struct {
	create   *managev1.CreateDocumentRequest
	update   *managev1.UpdateDocumentRequest
	del      *managev1.DeleteDocumentRequest
	transfer *managev1.TransferDocumentRequest
}

func (o operation) kind() managev1.DocumentKind {
	switch {
	case o.create != nil:
		return o.create.GetDocument().GetKind()
	case o.update != nil:
		return o.update.GetDocument().GetKind()
	case o.del != nil:
		return o.del.GetKind()
	}
	return o.transfer.GetKind()
}

func (o operation) write() *managev1.Write {
	switch {
	case o.create != nil:
		return &managev1.Write{Operation: &managev1.Write_Create{Create: o.create}}
	case o.update != nil:
		return &managev1.Write{Operation: &managev1.Write_Update{Update: o.update}}
	case o.del != nil:
		return &managev1.Write{Operation: &managev1.Write_Delete{Delete: o.del}}
	}
	return nil
}

type staged struct {
	docs    map[docKey]*managev1.Document
	changed map[docKey]bool
	created []string
	updated []string
	deleted []string
	impact  *managev1.Impact
}

// write runs ops atomically as principal. With approved set, the hold for
// approval is skipped (the change was approved).
func (s *Server) write(principal string, ops []operation, opts *managev1.WriteOptions, request proto.Message, approved bool) (*managev1.WriteResult, error) {
	c, err := s.caller(principal)
	if err != nil {
		return nil, err
	}
	var recordKey string
	var fp [32]byte
	if !opts.GetValidateOnly() && opts.GetRequestId() != "" && request != nil {
		recordKey = principal + "\x00" + opts.GetRequestId()
		body, _ := proto.MarshalOptions{Deterministic: true}.Marshal(request)
		fp = sha256.Sum256(body)
		if rec, ok := s.requests[recordKey]; ok {
			if rec.fingerprint != fp {
				return nil, apiError(codes.InvalidArgument, "REQUEST_ID_REUSED", "request_id was used with a different body", nil, 0)
			}
			return rec.result, rec.err
		}
	}
	res, err := s.writeOnce(c, ops, opts, approved)
	if recordKey != "" && (err == nil || status.Code(err) != codes.Unavailable) {
		s.requests[recordKey] = requestRecord{fingerprint: fp, result: res, err: err}
	}
	return res, err
}

func (s *Server) writeOnce(c caller, ops []operation, opts *managev1.WriteOptions, approved bool) (*managev1.WriteResult, error) {
	st := &staged{docs: map[docKey]*managev1.Document{}, changed: map[docKey]bool{}, impact: &managev1.Impact{}}
	for k, v := range s.docs {
		st.docs[k] = v
	}
	held := false
	for _, op := range ops {
		if err := s.apply(c, st, op); err != nil {
			return nil, err
		}
		if c.writer != nil && s.holdKinds[op.kind()] && op.del == nil && op.transfer == nil {
			held = true
		}
	}
	if held && !approved {
		st.impact.WidensStandingAccess = true
		if opts.GetFailIfApprovalNeeded() {
			return nil, apiError(codes.FailedPrecondition, "APPROVAL_REQUIRED", "this change widens standing access and needs approval", nil, 0)
		}
		if opts.GetValidateOnly() {
			return &managev1.WriteResult{Outcome: managev1.WriteOutcome_WRITE_OUTCOME_VALIDATED, PolicyVersion: s.version, Documents: st.results(), Impact: st.impact}, nil
		}
		s.changeSeq++
		ch := &managev1.PolicyChange{
			Id:                fmt.Sprintf("chg-%04d", s.changeSeq),
			State:             managev1.PolicyChange_STATE_PENDING,
			Author:            c.principal,
			AuthorCi:          map[string]string{"provider": "fake"},
			Impact:            st.impact,
			BasePolicyVersion: s.version,
			Reason:            opts.GetReason(),
			SourceRef:         opts.GetSourceRef(),
			CreateTimeUnix:    time.Now().Unix(),
			ExpireTimeUnix:    time.Now().Add(7 * 24 * time.Hour).Unix(),
		}
		for _, op := range ops {
			ch.Writes = append(ch.Writes, op.write())
		}
		s.changes[ch.Id] = ch
		return &managev1.WriteResult{Outcome: managev1.WriteOutcome_WRITE_OUTCOME_PENDING_APPROVAL, PolicyVersion: s.version, Impact: st.impact, PendingChange: proto.Clone(ch).(*managev1.PolicyChange)}, nil
	}
	if len(st.changed) == 0 {
		return &managev1.WriteResult{Outcome: managev1.WriteOutcome_WRITE_OUTCOME_UNCHANGED, PolicyVersion: s.version, Documents: st.results(), Impact: st.impact}, nil
	}
	if opts.GetValidateOnly() {
		return &managev1.WriteResult{Outcome: managev1.WriteOutcome_WRITE_OUTCOME_VALIDATED, PolicyVersion: s.version, Documents: st.results(), Impact: st.impact}, nil
	}
	s.version++
	var own *managev1.Ownership
	for k := range st.changed {
		if d := st.docs[k]; d != nil {
			s.stamp(d, c.principal)
			own = d.Ownership
		}
	}
	s.docs = st.docs
	s.versions = append(s.versions, &managev1.PolicyVersion{
		Version: s.version, Digest: randomHex(32), Author: c.principal, SourceRef: opts.GetSourceRef(),
		Ownership: own, CreateTimeUnix: time.Now().Unix(),
		Created: st.created, Updated: st.updated, Deleted: st.deleted,
	})
	return &managev1.WriteResult{Outcome: managev1.WriteOutcome_WRITE_OUTCOME_APPLIED, PolicyVersion: s.version, Documents: st.results(), Impact: st.impact}, nil
}

func (st *staged) results() []*managev1.Document {
	var out []*managev1.Document
	keys := make([]docKey, 0, len(st.changed))
	for k := range st.changed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
	for _, k := range keys {
		if d := st.docs[k]; d != nil {
			out = append(out, cloneDoc(d))
		}
	}
	return out
}

func (st *staged) touch(d *managev1.Document) docKey {
	k := docKey{d.Kind, d.Name}
	st.docs[k] = d
	st.changed[k] = true
	return k
}

func ownedElsewhere(o *managev1.Ownership) error {
	return apiError(codes.FailedPrecondition, "OWNED_ELSEWHERE",
		fmt.Sprintf("managed by %s source %q in namespace %q", strings.ToLower(strings.TrimPrefix(o.Channel.String(), "CHANNEL_")), o.Source, o.Namespace),
		map[string]string{"channel": o.Channel.String(), "source": o.Source, "namespace": o.Namespace}, 0)
}

func etagMismatch(current string) error {
	return apiError(codes.FailedPrecondition, "ETAG_MISMATCH", "the etag is stale", map[string]string{"current_etag": current}, 0)
}

func (s *Server) apply(c caller, st *staged, op operation) error {
	switch {
	case op.create != nil:
		return s.applyCreate(c, st, op.create)
	case op.update != nil:
		return s.applyUpdate(c, st, op.update)
	case op.del != nil:
		return s.applyDelete(c, st, op.del)
	}
	return s.applyTransfer(c, st, op.transfer)
}

func (s *Server) applyCreate(c caller, st *staged, req *managev1.CreateDocumentRequest) error {
	in := req.GetDocument()
	own := cloneOwnership(req.GetOwnership())
	if own == nil {
		own = &managev1.Ownership{}
	}
	own.Channel = managev1.Channel_CHANNEL_API
	if own.Namespace == "" {
		if c.writer != nil {
			return status.Error(codes.InvalidArgument, "ownership.namespace is required for a writer")
		}
		own.Namespace = "default"
	}
	if own.Source == "" {
		return status.Error(codes.InvalidArgument, "ownership.source is required")
	}
	if err := c.authorize(in.GetKind(), in.GetName(), own.Namespace); err != nil {
		return err
	}
	if c.writer != nil && in.GetKind() == managev1.DocumentKind_DOCUMENT_KIND_RESOURCE {
		if in.GetProfile() == "" {
			return outsideGrant("profile", "writers create resources only by completing a profile")
		}
		if !contains(c.writer.Profiles, in.GetProfile()) {
			return outsideGrant("profile", "the writer may not complete profile "+in.GetProfile())
		}
	}
	key := docKey{in.GetKind(), in.GetName()}
	if st.docs[key] != nil {
		return status.Errorf(codes.AlreadyExists, "%s/%s already exists", kindName(in.GetKind()), in.GetName())
	}
	if c.writer != nil && c.writer.MaxDocuments > 0 && s.countOwned(st, c.writer.Namespace) >= int(c.writer.MaxDocuments) {
		return apiError(codes.ResourceExhausted, "QUOTA_EXCEEDED", fmt.Sprintf("the writer owns %d documents already", c.writer.MaxDocuments), nil, 0)
	}
	d := &managev1.Document{
		Kind: in.GetKind(), Name: in.GetName(), Description: in.GetDescription(),
		Labels: in.GetLabels(), SpecJson: in.GetSpecJson(), Profile: in.GetProfile(), Ownership: own,
	}
	if err := s.complete(d, "create"); err != nil {
		return err
	}
	if c.writer != nil && d.Kind == managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING && strings.Contains(d.SpecJson, `"limen:`) {
		return outsideGrant("builtin_role", "writers may not name a built-in role")
	}
	st.touch(d)
	st.created = append(st.created, kindName(d.Kind)+"/"+d.Name)
	st.impact.AccessChanges = append(st.impact.AccessChanges, accessChanges(d, managev1.AccessChange_DIRECTION_GAIN)...)
	if len(st.impact.AccessChanges) > 0 {
		st.impact.WidensStandingAccess = true
	}
	return nil
}

func (s *Server) countOwned(st *staged, namespace string) int {
	n := 0
	for _, d := range st.docs {
		if d.Ownership.GetChannel() == managev1.Channel_CHANNEL_API && d.Ownership.GetNamespace() == namespace {
			n++
		}
	}
	return n
}

// accessChanges simulates the impact engine: whoever the resource's `team`
// label names gains or loses standing access to it.
func accessChanges(d *managev1.Document, dir managev1.AccessChange_Direction) []*managev1.AccessChange {
	var team string
	switch d.Kind {
	case managev1.DocumentKind_DOCUMENT_KIND_RESOURCE:
		team = d.EffectiveLabels["team"]
	case managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING:
		var rows []map[string]any
		_ = json.Unmarshal([]byte(d.SpecJson), &rows)
		if len(rows) > 0 {
			team, _ = rows[0]["idp_group"].(string)
		}
	}
	if team == "" {
		return nil
	}
	verb := "gains"
	if dir == managev1.AccessChange_DIRECTION_LOSS {
		verb = "loses"
	}
	subject := "group:" + team
	return []*managev1.AccessChange{{
		Direction: dir, Subject: subject, Resource: d.Name, Principal: "db role app_rw",
		Standing: true, ViaRole: team + "-access",
		Text: fmt.Sprintf("%s %s standing access to %s as db role app_rw (via role %s-access)", subject, verb, d.Name, team),
	}}
}

func (s *Server) existing(c caller, st *staged, kind managev1.DocumentKind, name string) (*managev1.Document, error) {
	d := st.docs[docKey{kind, name}]
	ns := ""
	if d != nil {
		ns = d.Ownership.GetNamespace()
	} else if c.writer != nil {
		ns = c.writer.Namespace
	}
	if err := c.authorize(kind, name, ns); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Server) applyUpdate(c caller, st *staged, req *managev1.UpdateDocumentRequest) error {
	in := req.GetDocument()
	cur, err := s.existing(c, st, in.GetKind(), in.GetName())
	if err != nil {
		return err
	}
	if cur == nil {
		return status.Errorf(codes.NotFound, "no %s/%s", kindName(in.GetKind()), in.GetName())
	}
	if cur.Ownership.GetChannel() != managev1.Channel_CHANNEL_API {
		return ownedElsewhere(cur.Ownership)
	}
	if req.GetEtag() != cur.Etag {
		return etagMismatch(cur.Etag)
	}
	if c.writer != nil && in.GetKind() == managev1.DocumentKind_DOCUMENT_KIND_RESOURCE && !contains(c.writer.Profiles, in.GetProfile()) {
		return outsideGrant("profile", "the writer may not complete profile "+in.GetProfile())
	}
	next := cloneDoc(cur)
	next.Description, next.Labels, next.SpecJson, next.Profile = in.GetDescription(), in.GetLabels(), in.GetSpecJson(), in.GetProfile()
	if err := s.complete(next, "update"); err != nil {
		return err
	}
	if sameContent(cur, next) {
		return nil
	}
	st.touch(next)
	st.updated = append(st.updated, kindName(next.Kind)+"/"+next.Name)
	return nil
}

func (s *Server) applyDelete(c caller, st *staged, req *managev1.DeleteDocumentRequest) error {
	cur, err := s.existing(c, st, req.GetKind(), req.GetName())
	if err != nil {
		return err
	}
	if cur == nil {
		if req.GetAllowMissing() {
			return nil
		}
		return status.Errorf(codes.NotFound, "no %s/%s", kindName(req.GetKind()), req.GetName())
	}
	if cur.Ownership.GetChannel() != managev1.Channel_CHANNEL_API {
		return ownedElsewhere(cur.Ownership)
	}
	if req.GetEtag() != cur.Etag && !(req.GetAllowMissing() && req.GetEtag() == "") {
		return etagMismatch(cur.Etag)
	}
	key := docKey{req.GetKind(), req.GetName()}
	delete(st.docs, key)
	st.changed[key] = true
	st.deleted = append(st.deleted, kindName(req.GetKind())+"/"+req.GetName())
	st.impact.AccessChanges = append(st.impact.AccessChanges, accessChanges(cur, managev1.AccessChange_DIRECTION_LOSS)...)
	return nil
}

func (s *Server) applyTransfer(c caller, st *staged, req *managev1.TransferDocumentRequest) error {
	cur, err := s.existing(c, st, req.GetKind(), req.GetName())
	if err != nil {
		return err
	}
	if cur == nil {
		return status.Errorf(codes.NotFound, "no %s/%s", kindName(req.GetKind()), req.GetName())
	}
	to := cloneOwnership(req.GetOwnership())
	to.Channel = managev1.Channel_CHANNEL_API
	if c.writer != nil {
		if cur.Ownership.GetChannel() != managev1.Channel_CHANNEL_API {
			return ownedElsewhere(cur.Ownership)
		}
		if to.Namespace != c.writer.Namespace {
			return outsideGrant("namespace", "writers move documents only between their own sources")
		}
	}
	if req.GetEtag() != cur.Etag {
		return etagMismatch(cur.Etag)
	}
	if proto.Equal(cur.Ownership, to) {
		return nil
	}
	next := cloneDoc(cur)
	next.Ownership = to
	st.touch(next)
	st.updated = append(st.updated, kindName(next.Kind)+"/"+next.Name)
	return nil
}

func (s *Server) decide(id string, approve bool, by, note string) (*managev1.PolicyChange, error) {
	ch := s.changes[id]
	if ch == nil {
		return nil, status.Errorf(codes.NotFound, "no change %s", id)
	}
	if ch.State != managev1.PolicyChange_STATE_PENDING || ch.Author == by {
		return nil, apiError(codes.FailedPrecondition, "CHANGE_NOT_DECIDABLE", "the change isn't pending, or you made it", nil, 0)
	}
	ch.DecidedBy, ch.DecisionNote, ch.DecideTimeUnix = by, note, time.Now().Unix()
	if !approve {
		ch.State = managev1.PolicyChange_STATE_REJECTED
		return proto.Clone(ch).(*managev1.PolicyChange), nil
	}
	var ops []operation
	for _, w := range ch.Writes {
		switch o := w.Operation.(type) {
		case *managev1.Write_Create:
			ops = append(ops, operation{create: o.Create})
		case *managev1.Write_Update:
			ops = append(ops, operation{update: o.Update})
		case *managev1.Write_Delete:
			ops = append(ops, operation{del: o.Delete})
		}
	}
	res, err := s.write(ch.Author, ops, &managev1.WriteOptions{SourceRef: ch.SourceRef}, nil, true)
	if err != nil {
		ch.State = managev1.PolicyChange_STATE_SUPERSEDED
		return proto.Clone(ch).(*managev1.PolicyChange), nil
	}
	ch.State = managev1.PolicyChange_STATE_APPLIED
	ch.AppliedPolicyVersion = res.PolicyVersion
	return proto.Clone(ch).(*managev1.PolicyChange), nil
}

// ---- The service ----------------------------------------------------------------

type manageServer struct {
	managev1.UnimplementedManagementServiceServer
	s *Server
}

func (m *manageServer) DescribeCaller(ctx context.Context, _ *managev1.DescribeCallerRequest) (*managev1.DescribeCallerResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p := principalFrom(ctx)
	resp := &managev1.DescribeCallerResponse{Principal: p, Admin: s.admins[p], PolicyVersion: s.version}
	if bot, ok := strings.CutPrefix(p, "bot:"); ok {
		if w := s.writers[bot]; w != nil {
			resp.Grant = &managev1.WriterGrant{
				Name: w.Name, Namespace: w.Namespace, NamePatterns: w.NamePatterns, Kinds: w.Kinds,
				Profiles: w.Profiles, MaxDocuments: w.MaxDocuments, ApproverGroups: w.ApproverGroups,
				DocumentCount: uint32(s.countOwned(&staged{docs: s.docs}, w.Namespace)),
			}
		}
	}
	return resp, nil
}

func (s *Server) mayRead(c caller, d *managev1.Document) bool {
	if c.writer == nil {
		return true
	}
	if d.Kind == managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE {
		return contains(c.writer.Profiles, d.Name)
	}
	return d.Ownership.GetNamespace() == c.writer.Namespace
}

func (m *manageServer) GetDocument(ctx context.Context, req *managev1.GetDocumentRequest) (*managev1.GetDocumentResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.caller(principalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if req.GetKind() == managev1.DocumentKind_DOCUMENT_KIND_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "kind is required")
	}
	d := s.docs[docKey{req.GetKind(), req.GetName()}]
	if d == nil {
		return nil, status.Errorf(codes.NotFound, "no %s/%s", kindName(req.GetKind()), req.GetName())
	}
	if !s.mayRead(c, d) {
		return nil, outsideGrant("namespace", "outside the writer's namespace")
	}
	return &managev1.GetDocumentResponse{Document: cloneDoc(d)}, nil
}

func (m *manageServer) ListDocuments(ctx context.Context, req *managev1.ListDocumentsRequest) (*managev1.ListDocumentsResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.caller(principalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if req.GetKind() == managev1.DocumentKind_DOCUMENT_KIND_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "kind is required")
	}
	var all []*managev1.Document
	for k, d := range s.docs {
		if k.kind != req.GetKind() || !s.mayRead(c, d) {
			continue
		}
		if req.GetNamespace() != "" && d.Ownership.GetNamespace() != req.GetNamespace() {
			continue
		}
		if req.GetSource() != "" && d.Ownership.GetSource() != req.GetSource() {
			continue
		}
		match := true
		for lk, lv := range req.GetLabelSelector() {
			if d.EffectiveLabels[lk] != lv {
				match = false
			}
		}
		if match {
			all = append(all, d)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	size := int(req.GetPageSize())
	if size <= 0 {
		size = 100
	}
	if size > 500 {
		return nil, status.Error(codes.InvalidArgument, "page_size is at most 500")
	}
	start := 0
	if t := req.GetPageToken(); t != "" {
		if start, err = strconv.Atoi(t); err != nil || start < 0 || start > len(all) {
			return nil, status.Error(codes.InvalidArgument, "bad page token")
		}
	}
	end := min(start+size, len(all))
	resp := &managev1.ListDocumentsResponse{PolicyVersion: s.version}
	for _, d := range all[start:end] {
		resp.Documents = append(resp.Documents, cloneDoc(d))
	}
	if end < len(all) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	return resp, nil
}

func (m *manageServer) CreateDocument(ctx context.Context, req *managev1.CreateDocumentRequest) (*managev1.CreateDocumentResponse, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	res, err := m.s.write(principalFrom(ctx), []operation{{create: req}}, req.GetOptions(), req, false)
	if err != nil {
		return nil, err
	}
	return &managev1.CreateDocumentResponse{Result: res}, nil
}

func (m *manageServer) UpdateDocument(ctx context.Context, req *managev1.UpdateDocumentRequest) (*managev1.UpdateDocumentResponse, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	res, err := m.s.write(principalFrom(ctx), []operation{{update: req}}, req.GetOptions(), req, false)
	if err != nil {
		return nil, err
	}
	return &managev1.UpdateDocumentResponse{Result: res}, nil
}

func (m *manageServer) DeleteDocument(ctx context.Context, req *managev1.DeleteDocumentRequest) (*managev1.DeleteDocumentResponse, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	res, err := m.s.write(principalFrom(ctx), []operation{{del: req}}, req.GetOptions(), req, false)
	if err != nil {
		return nil, err
	}
	return &managev1.DeleteDocumentResponse{Result: res}, nil
}

func (m *manageServer) TransferDocument(ctx context.Context, req *managev1.TransferDocumentRequest) (*managev1.TransferDocumentResponse, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	res, err := m.s.write(principalFrom(ctx), []operation{{transfer: req}}, req.GetOptions(), req, false)
	if err != nil {
		return nil, err
	}
	return &managev1.TransferDocumentResponse{Result: res}, nil
}

func (m *manageServer) BatchWrite(ctx context.Context, req *managev1.BatchWriteRequest) (*managev1.BatchWriteResponse, error) {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if n := len(req.GetWrites()); n == 0 || n > 100 {
		return nil, status.Error(codes.InvalidArgument, "a batch has 1 to 100 writes")
	}
	var ops []operation
	for _, w := range req.GetWrites() {
		switch o := w.Operation.(type) {
		case *managev1.Write_Create:
			ops = append(ops, operation{create: o.Create})
		case *managev1.Write_Update:
			ops = append(ops, operation{update: o.Update})
		case *managev1.Write_Delete:
			ops = append(ops, operation{del: o.Delete})
		default:
			return nil, status.Error(codes.InvalidArgument, "empty write")
		}
	}
	res, err := m.s.write(principalFrom(ctx), ops, req.GetOptions(), req, false)
	if err != nil {
		return nil, err
	}
	return &managev1.BatchWriteResponse{Result: res}, nil
}

func (m *manageServer) GetChange(ctx context.Context, req *managev1.GetChangeRequest) (*managev1.GetChangeResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.caller(principalFrom(ctx))
	if err != nil {
		return nil, err
	}
	ch := s.changes[req.GetId()]
	if ch == nil || c.writer != nil && ch.Author != c.principal {
		return nil, status.Errorf(codes.NotFound, "no change %s", req.GetId())
	}
	return &managev1.GetChangeResponse{Change: proto.Clone(ch).(*managev1.PolicyChange)}, nil
}

func (m *manageServer) ListChanges(ctx context.Context, req *managev1.ListChangesRequest) (*managev1.ListChangesResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.caller(principalFrom(ctx))
	if err != nil {
		return nil, err
	}
	want := req.GetState()
	if want == managev1.PolicyChange_STATE_UNSPECIFIED {
		want = managev1.PolicyChange_STATE_PENDING
	}
	resp := &managev1.ListChangesResponse{}
	for _, ch := range s.changes {
		if ch.State == want && (c.writer == nil || ch.Author == c.principal) {
			resp.Changes = append(resp.Changes, proto.Clone(ch).(*managev1.PolicyChange))
		}
	}
	sort.Slice(resp.Changes, func(i, j int) bool { return resp.Changes[i].Id > resp.Changes[j].Id })
	return resp, nil
}

func (m *manageServer) ApproveChange(ctx context.Context, req *managev1.ApproveChangeRequest) (*managev1.ApproveChangeResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p := principalFrom(ctx)
	if !s.admins[p] {
		return nil, status.Error(codes.PermissionDenied, "only people decide changes")
	}
	ch, err := s.decide(req.GetId(), true, p, req.GetNote())
	if err != nil {
		return nil, err
	}
	return &managev1.ApproveChangeResponse{Change: ch}, nil
}

func (m *manageServer) RejectChange(ctx context.Context, req *managev1.RejectChangeRequest) (*managev1.RejectChangeResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p := principalFrom(ctx)
	if !s.admins[p] {
		return nil, status.Error(codes.PermissionDenied, "only people decide changes")
	}
	ch, err := s.decide(req.GetId(), false, p, req.GetNote())
	if err != nil {
		return nil, err
	}
	return &managev1.RejectChangeResponse{Change: ch}, nil
}

func (m *manageServer) CancelChange(ctx context.Context, req *managev1.CancelChangeRequest) (*managev1.CancelChangeResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.changes[req.GetId()]
	if ch == nil || ch.Author != principalFrom(ctx) || ch.State != managev1.PolicyChange_STATE_PENDING {
		return nil, apiError(codes.FailedPrecondition, "CHANGE_NOT_DECIDABLE", "only a pending change's author cancels it", nil, 0)
	}
	ch.State = managev1.PolicyChange_STATE_CANCELLED
	return &managev1.CancelChangeResponse{Change: proto.Clone(ch).(*managev1.PolicyChange)}, nil
}

func (m *manageServer) ListPolicyVersions(ctx context.Context, _ *managev1.ListPolicyVersionsRequest) (*managev1.ListPolicyVersionsResponse, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.caller(principalFrom(ctx)); err != nil {
		return nil, err
	}
	resp := &managev1.ListPolicyVersionsResponse{}
	for i := len(s.versions) - 1; i >= 0; i-- {
		resp.Versions = append(resp.Versions, proto.Clone(s.versions[i]).(*managev1.PolicyVersion))
	}
	return resp, nil
}

func (m *manageServer) GetSchema(_ context.Context, req *managev1.GetSchemaRequest) (*managev1.GetSchemaResponse, error) {
	if req.GetKind() == managev1.DocumentKind_DOCUMENT_KIND_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "kind is required")
	}
	typ := "object"
	if req.GetKind() == managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING {
		typ = "array"
	}
	schema := fmt.Sprintf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","title":%q,"type":%q}`, kindName(req.GetKind()), typ)
	return &managev1.GetSchemaResponse{JsonSchema: schema, ServerVersion: m.s.opts.ServerVersion}, nil
}
