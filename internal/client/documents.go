package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

// Kinds by their YAML name.
var kindsByName = map[string]managev1.DocumentKind{
	"role":             managev1.DocumentKind_DOCUMENT_KIND_ROLE,
	"role_mapping":     managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING,
	"resource":         managev1.DocumentKind_DOCUMENT_KIND_RESOURCE,
	"bot":              managev1.DocumentKind_DOCUMENT_KIND_BOT,
	"discovery":        managev1.DocumentKind_DOCUMENT_KIND_DISCOVERY,
	"policy_test":      managev1.DocumentKind_DOCUMENT_KIND_POLICY_TEST,
	"resource_profile": managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE,
	"policy_writer":    managev1.DocumentKind_DOCUMENT_KIND_POLICY_WRITER,
}

// KindNames are the YAML names of every kind, in declaration order.
var KindNames = []string{"role", "role_mapping", "resource", "bot", "discovery", "policy_test", "resource_profile", "policy_writer"}

// ParseKind turns a YAML kind name into the enum.
func ParseKind(name string) (managev1.DocumentKind, error) {
	k, ok := kindsByName[name]
	if !ok {
		return 0, fmt.Errorf("unknown kind %q: one of %s", name, strings.Join(KindNames, ", "))
	}
	return k, nil
}

// KindName is the YAML name of a kind.
func KindName(k managev1.DocumentKind) string {
	for n, v := range kindsByName {
		if v == k {
			return n
		}
	}
	return strings.ToLower(strings.TrimPrefix(k.String(), "DOCUMENT_KIND_"))
}

// ChannelName is the lowercase name of a channel: git, console, or api.
func ChannelName(c managev1.Channel) string {
	if c == managev1.Channel_CHANNEL_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(c.String(), "CHANNEL_"))
}

// NewRequestID is a random UUID (version 4). One is made per write and kept
// across retries of that write, so Limen applies it at most once.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// GetDocument reads one document from the active policy. A missing document
// is (nil, nil).
func (c *Client) GetDocument(ctx context.Context, kind managev1.DocumentKind, name string) (*managev1.Document, error) {
	resp, err := c.Manage.GetDocument(ctx, &managev1.GetDocumentRequest{Kind: kind, Name: name})
	if IsCode(err, codes.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return resp.GetDocument(), nil
}

// ListFilter selects documents for ListAll.
type ListFilter struct {
	Kind      managev1.DocumentKind
	Namespace string
	Source    string
	Labels    map[string]string
}

// ListAll reads every page of a listing, which Limen serves from one policy
// version.
func (c *Client) ListAll(ctx context.Context, f ListFilter) ([]*managev1.Document, int64, error) {
	var (
		out     []*managev1.Document
		token   string
		version int64
	)
	for {
		resp, err := c.Manage.ListDocuments(ctx, &managev1.ListDocumentsRequest{
			Kind:          f.Kind,
			Namespace:     f.Namespace,
			Source:        f.Source,
			LabelSelector: f.Labels,
			PageSize:      500,
			PageToken:     token,
		})
		if err != nil {
			return nil, 0, err
		}
		out = append(out, resp.GetDocuments()...)
		version = resp.GetPolicyVersion()
		token = resp.GetNextPageToken()
		if token == "" {
			return out, version, nil
		}
	}
}

// ErrPending is returned by a write that Limen held for approval, when the
// provider was not asked to wait or the wait ended first.
type ErrPending struct {
	Change *managev1.PolicyChange
	Waited time.Duration
}

func (e *ErrPending) Error() string {
	return fmt.Sprintf("change %s is waiting for approval", e.Change.GetId())
}

// ErrChangeDecided is returned when a held change ends in any state but APPLIED.
type ErrChangeDecided struct {
	Change *managev1.PolicyChange
}

func (e *ErrChangeDecided) Error() string {
	ch := e.Change
	state := strings.ToLower(strings.TrimPrefix(ch.GetState().String(), "STATE_"))
	msg := fmt.Sprintf("change %s was %s", ch.GetId(), state)
	if ch.GetDecidedBy() != "" {
		msg += " by " + ch.GetDecidedBy()
	}
	if ch.GetDecisionNote() != "" {
		msg += ": " + ch.GetDecisionNote()
	}
	return msg
}

// WaitForChange polls a held change until it is decided or wait runs out.
// It returns the change once APPLIED, an *ErrChangeDecided for any other
// decision, or an *ErrPending when wait ends first.
func (c *Client) WaitForChange(ctx context.Context, change *managev1.PolicyChange, wait, every time.Duration) (*managev1.PolicyChange, error) {
	deadline := time.Now().Add(wait)
	start := time.Now()
	for {
		switch change.GetState() {
		case managev1.PolicyChange_STATE_APPLIED:
			return change, nil
		case managev1.PolicyChange_STATE_PENDING, managev1.PolicyChange_STATE_UNSPECIFIED:
		default:
			return nil, &ErrChangeDecided{Change: change}
		}
		if !time.Now().Add(every).Before(deadline) {
			return nil, &ErrPending{Change: change, Waited: time.Since(start).Round(time.Second)}
		}
		if err := c.cfg.Retry.Sleep(ctx, every); err != nil {
			return nil, &ErrPending{Change: change, Waited: time.Since(start).Round(time.Second)}
		}
		resp, err := c.Manage.GetChange(ctx, &managev1.GetChangeRequest{Id: change.GetId()})
		if err != nil {
			return nil, err
		}
		change = resp.GetChange()
	}
}

// IsNotFound reports a NOT_FOUND error.
func IsNotFound(err error) bool { return IsCode(err, codes.NotFound) }

// IsAlreadyExists reports an ALREADY_EXISTS error.
func IsAlreadyExists(err error) bool { return IsCode(err, codes.AlreadyExists) }

// AsPending returns the held change from err, if it is one.
func AsPending(err error) (*ErrPending, bool) {
	var p *ErrPending
	ok := errors.As(err, &p)
	return p, ok
}
