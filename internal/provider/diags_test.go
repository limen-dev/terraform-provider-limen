package provider

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

func TestImpactWarnings(t *testing.T) {
	var diags diag.Diagnostics
	addImpact(&diags, "role/payments-read", &managev1.Impact{
		WidensStandingAccess: true,
		AccessChanges: []*managev1.AccessChange{
			{Direction: managev1.AccessChange_DIRECTION_GAIN, Subject: "group:payments-eng", Resource: "orders", Principal: "db role limen_read", Standing: true, ViaRole: "payments-read"},
			{Direction: managev1.AccessChange_DIRECTION_LOSS, Text: "user:bob loses standing access to orders"},
		},
		AccessChangesTruncated: true,
		Tests:                  []*managev1.TestResult{{Test: "locked", CaseNumber: 2, Name: "no root", Expected: "deny", Got: "allow", Passed: false}, {Test: "ok", Passed: true}},
		Warnings:               []string{"the role selects no resource"},
	})
	if len(diags) != 3 || diags.HasError() {
		t.Fatalf("want three warnings, got %v", diags)
	}
	access := diags[0]
	if access.Summary() != "Access changes (widens standing access): role/payments-read" {
		t.Fatal(access.Summary())
	}
	want := "+ group:payments-eng gains standing access to orders as db role limen_read (via role payments-read)\n" +
		"- user:bob loses standing access to orders\n… and more (Limen lists at most 500)"
	if access.Detail() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", access.Detail(), want)
	}
	if !strings.Contains(diags[1].Detail(), "locked case 2 (no root): expected deny, got allow") {
		t.Fatal(diags[1].Detail())
	}
	if diags[2].Detail() != "the role selects no resource" {
		t.Fatal(diags[2].Detail())
	}
}

func TestPendingAndDecidedErrors(t *testing.T) {
	var diags diag.Diagnostics
	addError(&diags, "Creating role/x", &client.ErrPending{Change: &managev1.PolicyChange{Id: "chg-9"}, Replaced: []string{"chg-8"}})
	if diags[0].Summary() != "Change is waiting for approval" || !strings.Contains(diags[0].Detail(), "replaces: chg-8") {
		t.Fatal(diags[0].Detail())
	}
	diags = nil
	wrapped := errors.Join(errors.New("ctx"), &client.ErrChangeDecided{Change: &managev1.PolicyChange{Id: "chg-9", State: managev1.PolicyChange_STATE_SUPERSEDED}})
	addError(&diags, "Creating role/x", wrapped)
	if diags[0].Summary() != "Change was not applied" || !strings.Contains(diags[0].Detail(), "chg-9 was superseded") {
		t.Fatal(diags[0].Detail())
	}
}

func TestPollInterval(t *testing.T) {
	for wait, want := range map[string]string{"5s": "1s", "1m": "2s", "30m": "10s"} {
		w, _ := time.ParseDuration(wait)
		if got := pollInterval(w).String(); got != want {
			t.Errorf("%s: %s, want %s", wait, got, want)
		}
	}
}
