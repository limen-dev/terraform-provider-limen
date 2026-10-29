package provider

import (
	managev1 "github.com/limen-dev/terraform-provider-limen/internal/limenpb/limen/manage/v1"
)

// kindInfo describes one kind of policy document for its resource and data
// source.
type kindInfo struct {
	kind managev1.DocumentKind
	// name is the YAML kind and the type name's suffix: limen_<name>.
	name string
	// what is a short noun phrase for descriptions.
	what string
	// doc is a paragraph about the kind, for both the resource and the data source.
	doc string
	// specDoc describes the spec attribute.
	specDoc string
	// profile: the kind has the `profile` attribute (resources only).
	profile bool
}

var (
	kindResource = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE,
		name: "resource",
		what: "resource",
		doc: "A Limen resource: something people connect to through Limen, such as a database, an SSH host, a Kubernetes cluster, or a web app.\n\n" +
			"A bot writer never writes a full resource. It **completes a resource profile** that a person wrote: the profile fixes the credentials, " +
			"the worker, IAM, and audit settings, and names the few fields the writer may set (usually the endpoint) and their bounds (ADR-0042 §5). " +
			"Creating a resource from a profile is preapproved, so a team's module can create a database and register it in Limen in the same apply.",
		specDoc: "The resource's `spec`, in exactly the shape of the YAML spec, for example `{ postgres = { endpoint = aws_db_instance.orders.endpoint } }`. " +
			"With `profile`, set only the fields the profile makes settable; the rest comes from the profile and is in `effective_spec_json`. " +
			"`yamldecode(file(\"orders.yaml\"))` works too.",
		profile: true,
	}
	kindRole = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_ROLE,
		name: "role",
		what: "role",
		doc: "A Limen role: allow and deny rules that select resources by kind, name, and labels, and say how they may be used " +
			"(SSH logins, database roles, Kubernetes groups, and so on).\n\n" +
			"Bot writers may write roles only in their namespace, never naming a built-in role; a change that widens standing access is held " +
			"for a person to approve (ADR-0042 §4 and §6).",
		specDoc: "The role's `spec`, in exactly the shape of the YAML spec: `allow`, `deny`, `cert_ttl`, `max_session_ttl`, and so on.",
	}
	kindRoleMapping = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_ROLE_MAPPING,
		name: "role_mapping",
		what: "role mapping",
		doc: "A Limen role mapping: which IdP groups get which roles.\n\n" +
			"Writers can never assign a built-in role (`limen:admin`, `limen:auditor`), and a mapping that widens standing access is held for approval.",
		specDoc: "The mapping's `spec`: a list of `{ idp_group = \"...\", roles = [\"...\"] }`, in exactly the shape of the YAML spec.",
	}
	kindPolicyTest = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_POLICY_TEST,
		name: "policy_test",
		what: "policy test",
		doc: "A Limen policy test: cases that state who must or must not reach what (ADR-0028). " +
			"Every write keeps the policy's tests passing when the control plane runs with `--policy-tests=required`, " +
			"and a writer can't delete a test it doesn't own.",
		specDoc: "The test's `spec`: `cases`, each with `name`, `who`, `resource`, `principal`, and `expect`, in exactly the shape of the YAML spec.",
	}
	kindBot = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_BOT,
		name: "bot",
		what: "bot",
		doc: "A Limen bot: a machine identity that logs in with a CI platform's workload identity token (ADR-0019). " +
			"Bots are written only by people, through Git or the console, so Terraform can read them but not write them.",
	}
	kindDiscovery = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_DISCOVERY,
		name: "discovery",
		what: "discovery rule",
		doc:  "A Limen discovery rule (ADR-0027). Written only by people, through Git or the console.",
	}
	kindResourceProfile = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_RESOURCE_PROFILE,
		name: "resource_profile",
		what: "resource profile",
		doc: "A Limen resource profile: the fixed half of a resource, which writers complete (ADR-0042 §5). " +
			"Its spec has the `template` a writer can't change, the `settable` fields and their bounds, and the label rules. " +
			"Written only by people, through Git or the console. A bot writer may read the profiles its grant names.",
	}
	kindPolicyWriter = kindInfo{
		kind: managev1.DocumentKind_DOCUMENT_KIND_POLICY_WRITER,
		name: "policy_writer",
		what: "policy writer grant",
		doc: "A Limen policy writer: what one bot may write (ADR-0042 §4). Written only by people, through Git or the console; " +
			"no grant can include this kind. A bot sees its own grant with the `limen_caller` data source.",
	}
)

// writableKinds are the kinds Terraform may write: everything except the
// people-only kinds.
var writableKinds = []kindInfo{kindResource, kindRole, kindRoleMapping, kindPolicyTest}

// allKinds have data sources.
var allKinds = []kindInfo{kindResource, kindRole, kindRoleMapping, kindPolicyTest, kindBot, kindDiscovery, kindResourceProfile, kindPolicyWriter}
