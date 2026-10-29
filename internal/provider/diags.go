package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

// addError explains err as an error diagnostic, prefixed with what was being done.
func addError(diags *diag.Diagnostics, doing string, err error) {
	if p, ok := client.AsPending(err); ok {
		diags.AddError("Change is waiting for approval", pendingDetail(doing, p))
		return
	}
	var decided *client.ErrChangeDecided
	if errors.As(err, &decided) {
		diags.AddError("Change was not applied", fmt.Sprintf("%s: %s.\n\nNothing was changed. Fix the change and apply again.", doing, decided.Error()))
		return
	}
	summary, detail := client.Explain(err)
	diags.AddError(summary, doing+":\n\n"+detail)
}

func pendingDetail(doing string, p *client.ErrPending) string {
	ch := p.Change
	var b strings.Builder
	fmt.Fprintf(&b, "%s: Limen held the change because it widens standing access, so a person has to approve it.\n\n", doing)
	fmt.Fprintf(&b, "  change:   %s\n", ch.GetId())
	if ch.GetExpireTimeUnix() > 0 {
		fmt.Fprintf(&b, "  expires:  %s\n", time.Unix(ch.GetExpireTimeUnix(), 0).UTC().Format(time.RFC3339))
	}
	if p.Waited > 0 {
		fmt.Fprintf(&b, "  waited:   %s (approval_timeout)\n", p.Waited)
	}
	if lines := accessLines(ch.GetImpact()); len(lines) > 0 {
		b.WriteString("\nWhat it does:\n")
		for _, l := range lines {
			b.WriteString("  " + l + "\n")
		}
	}
	fmt.Fprintf(&b, "\nAn admin or one of the writer's approvers can review it with `limen policy changes show %s` and approve it in the console "+
		"or with `limen policy changes approve %s`. Then run terraform apply again: it finds the document in place. "+
		"To wait instead of failing, set the provider's approval_timeout.", ch.GetId(), ch.GetId())
	return b.String()
}

func accessLines(im *managev1.Impact) []string {
	var lines []string
	for _, ac := range im.GetAccessChanges() {
		text := ac.GetText()
		if text == "" {
			verb := "gains"
			if ac.GetDirection() == managev1.AccessChange_DIRECTION_LOSS {
				verb = "loses"
			}
			kind := "requestable"
			if ac.GetStanding() {
				kind = "standing"
			}
			text = fmt.Sprintf("%s %s %s access to %s as %s (via role %s)", ac.GetSubject(), verb, kind, ac.GetResource(), ac.GetPrincipal(), ac.GetViaRole())
		}
		sign := "+"
		if ac.GetDirection() == managev1.AccessChange_DIRECTION_LOSS {
			sign = "-"
		}
		lines = append(lines, sign+" "+text)
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i][0] < lines[j][0] })
	if im.GetAccessChangesTruncated() {
		lines = append(lines, "… and more (Limen lists at most 500)")
	}
	return lines
}

// addImpact shows what a dry run said a change would do, as warnings, so it
// is in the plan output next to the diff.
func addImpact(diags *diag.Diagnostics, address string, im *managev1.Impact) {
	if im == nil {
		return
	}
	if lines := accessLines(im); len(lines) > 0 {
		title := "Access changes"
		if im.GetWidensStandingAccess() {
			title = "Access changes (widens standing access)"
		}
		diags.AddWarning(fmt.Sprintf("%s: %s", title, address), strings.Join(lines, "\n"))
	}
	var failed []string
	for _, t := range im.GetTests() {
		if t.GetPassed() {
			continue
		}
		line := fmt.Sprintf("%s case %d (%s): expected %s, got %s", t.GetTest(), t.GetCaseNumber(), t.GetName(), t.GetExpected(), t.GetGot())
		if t.GetDetail() != "" {
			line += ": " + t.GetDetail()
		}
		failed = append(failed, line)
	}
	if len(failed) > 0 {
		diags.AddWarning("Policy tests would fail: "+address,
			strings.Join(failed, "\n")+"\n\nThe control plane runs tests in advisory mode, so the change is allowed. With --policy-tests=required it would be refused.")
	}
	for _, w := range im.GetWarnings() {
		diags.AddWarning("Limen: "+address, w)
	}
}
