package client

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrorDomain is the `google.rpc.ErrorInfo.domain` of the management API.
const ErrorDomain = "manage.limen.dev"

// Reasons the management API sends in `google.rpc.ErrorInfo.reason`
// (`limen.manage.v1.ErrorReason` without its prefix).
const (
	ReasonEtagMismatch          = "ETAG_MISMATCH"
	ReasonOwnedElsewhere        = "OWNED_ELSEWHERE"
	ReasonOutsideGrant          = "OUTSIDE_GRANT"
	ReasonInvalidDocument       = "INVALID_DOCUMENT"
	ReasonTestsFailed           = "TESTS_FAILED"
	ReasonApprovalRequired      = "APPROVAL_REQUIRED"
	ReasonPolicyVersionMismatch = "POLICY_VERSION_MISMATCH"
	ReasonRequestIDReused       = "REQUEST_ID_REUSED"
	ReasonQuotaExceeded         = "QUOTA_EXCEEDED"
	ReasonRateLimited           = "RATE_LIMITED"
	ReasonInUse                 = "IN_USE"
	ReasonAuditUnavailable      = "AUDIT_UNAVAILABLE"
	ReasonChangeNotDecidable    = "CHANGE_NOT_DECIDABLE"
)

// FieldViolation is one entry of a `google.rpc.BadRequest` detail.
type FieldViolation struct {
	Field       string
	Description string
}

// APIError is a gRPC error from Limen with its structured details.
type APIError struct {
	Code       codes.Code
	Message    string
	Reason     string
	Metadata   map[string]string
	Violations []FieldViolation
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(e.Message)
	if e.Message == "" {
		b.WriteString(e.Code.String())
	}
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "\n  - %s: %s", v.Field, v.Description)
	}
	return b.String()
}

// Is reports whether err is an APIError with the given reason.
func Is(err error, reason string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Reason == reason
}

// IsCode reports whether err is a gRPC error with the given code.
func IsCode(err error, code codes.Code) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == code
	}
	return status.Code(err) == code
}

// FromStatus turns a gRPC error into an *APIError. Errors that aren't gRPC
// statuses are returned unchanged.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	out := &APIError{Code: st.Code(), Message: st.Message()}
	for _, d := range st.Details() {
		switch d := d.(type) {
		case *errdetails.ErrorInfo:
			if d.GetDomain() == ErrorDomain || out.Reason == "" {
				out.Reason = d.GetReason()
				out.Metadata = d.GetMetadata()
			}
		case *errdetails.BadRequest:
			for _, v := range d.GetFieldViolations() {
				out.Violations = append(out.Violations, FieldViolation{Field: v.GetField(), Description: v.GetDescription()})
			}
		case *errdetails.RetryInfo:
			out.RetryAfter = d.GetRetryDelay().AsDuration()
		}
	}
	return out
}

// Explain returns a summary and a detail for err, worded for a Terraform
// user: what went wrong and what to do about it. The detail names the reason
// so it can be searched for.
func Explain(err error) (summary, detail string) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		msg := err.Error()
		if hint := transportHint(msg); hint != "" {
			return "Can't reach Limen", msg + "\n\n" + hint
		}
		return "Limen request failed", msg
	}
	md := apiErr.Metadata
	detail = apiErr.Error()
	tail := func(hint string) string {
		if apiErr.Reason != "" {
			return fmt.Sprintf("%s\n\n%s\n\n(reason: %s)", detail, hint, apiErr.Reason)
		}
		return detail + "\n\n" + hint
	}
	switch apiErr.Reason {
	case ReasonEtagMismatch:
		return "Document changed in Limen since Terraform read it",
			tail("Someone changed it after this plan was made. Run terraform plan again to see the current content; Limen never merges concurrent writes.")
	case ReasonOwnedElsewhere:
		return "Document is managed somewhere else",
			tail(fmt.Sprintf("It is owned by channel %q, source %q, namespace %q, and only its owner may change it. Change it there, or ask an admin to transfer it (limen policy transfer).",
				channelWord(md["channel"]), md["source"], md["namespace"]))
	case ReasonOutsideGrant:
		return "Outside this writer's grant",
			tail(grantHint(md["rule"]))
	case ReasonInvalidDocument:
		return "Limen refused the document as invalid",
			tail("Fix the fields named above. The document's spec has exactly the shape of the YAML spec (see the limen_schema data source).")
	case ReasonTestsFailed:
		return "The policy's tests fail after this change",
			tail("The control plane requires every policy_test to pass (--policy-tests=required). Fix the change or the test.")
	case ReasonApprovalRequired:
		return "This change needs approval",
			tail("It widens standing access, so Limen would hold it for a person to approve.")
	case ReasonPolicyVersionMismatch:
		return "Policy changed during the write", tail("Run terraform apply again.")
	case ReasonRequestIDReused:
		return "Request id reused with a different body",
			tail("This is a bug in the provider; please report it at https://github.com/limen-dev/terraform-provider-limen/issues.")
	case ReasonQuotaExceeded:
		return "Writer quota reached",
			tail("The writer already owns its grant's max_documents. An admin can raise max_documents on its policy_writer document.")
	case ReasonRateLimited:
		return "Too many writes",
			tail("The provider retried until its deadline. Lower terraform's -parallelism, or ask an admin about the control plane's write rate limit.")
	case ReasonInUse:
		return "Still in use",
			tail(fmt.Sprintf("These documents use it: %s. Remove or change them first.", md["used_by"]))
	case ReasonAuditUnavailable:
		return "Limen's audit trail is unavailable",
			tail("The change was not made, because Limen never makes a change it can't audit. Retry later.")
	case ReasonChangeNotDecidable:
		return "Change can't be decided", tail("It isn't pending, or you made it.")
	}
	switch apiErr.Code {
	case codes.Unauthenticated:
		return "Not authenticated to Limen", tail("Check the provider's bot or client_certificate settings; certificates last at most an hour.")
	case codes.PermissionDenied:
		return "Permission denied by Limen", tail("The caller may not do this. The limen_caller data source shows what it may write.")
	case codes.NotFound:
		return "Not found in Limen", detail
	case codes.Unavailable:
		if hint := transportHint(detail); hint != "" {
			return "Can't reach Limen", detail + "\n\n" + hint
		}
		return "Limen is unavailable", tail("The provider retried until its deadline.")
	case codes.Unimplemented:
		return "This Limen doesn't serve the management API",
			tail("The provider needs a control plane that serves limen.manage.v1 (ADR-0042, Phase 14), " +
				"behind a proxy that relays /limen.manage.v1.ManagementService/. Upgrade Limen; the provider's README lists supported versions.")
	}
	return "Limen request failed", detail
}

// channelWord turns `CHANNEL_GIT` or `git` into `git`.
func channelWord(c string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ToUpper(c), "CHANNEL_"))
}

func grantHint(rule string) string {
	switch rule {
	case "namespace":
		return "The document's namespace isn't the writer's. Leave namespace unset to use the writer's own."
	case "name_pattern":
		return "The name doesn't match any of the writer's name patterns (see limen_caller.grant.name_patterns)."
	case "kind":
		return "The writer may not write this kind. bot, discovery, resource_profile, and policy_writer are written only by people."
	case "profile":
		return "The writer may not complete this profile (see limen_caller.grant.profiles)."
	case "label":
		return "A label isn't allowed by the profile: its fixed labels can't be set, and the others must take an allowed value."
	case "field":
		return "The spec sets a field the profile doesn't make settable, or a value outside its bounds. Set only the profile's settable fields."
	case "builtin_role":
		return "Writers can never name a built-in role (limen:admin, limen:auditor)."
	}
	return "The limen_caller data source shows the writer's grant."
}

func transportHint(msg string) string {
	switch {
	case strings.Contains(msg, "certificate signed by unknown authority"),
		strings.Contains(msg, "x509:"):
		return "The proxy's certificate isn't signed by the CA the provider trusts. Set ca_certificate (or LIMEN_CA_FILE) to the proxy's CA."
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"):
		return "Check the provider's address (host:port of a Limen proxy)."
	}
	return ""
}

// SortedMetadata formats metadata deterministically, for logs.
func SortedMetadata(md map[string]string) string {
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+md[k])
	}
	return strings.Join(parts, " ")
}
