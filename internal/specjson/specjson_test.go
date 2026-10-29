package specjson

import (
	"context"
	"math/big"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func obj(t *testing.T, attrs map[string]attr.Value) types.Object {
	t.Helper()
	ts := map[string]attr.Type{}
	for k, v := range attrs {
		ts[k] = v.Type(context.Background())
	}
	o, d := types.ObjectValue(ts, attrs)
	if d.HasError() {
		t.Fatal(d)
	}
	return o
}

func TestFromValueIsCanonical(t *testing.T) {
	ctx := context.Background()
	tup, _ := types.TupleValue(
		[]attr.Type{types.StringType, types.NumberType},
		[]attr.Value{types.StringValue("a<b>&"), types.NumberValue(big.NewFloat(0.5))},
	)
	v := types.DynamicValue(obj(t, map[string]attr.Value{
		"zeta":  types.NumberValue(big.NewFloat(5432)),
		"alpha": types.BoolValue(true),
		"unset": types.StringNull(),
		"list":  tup,
		"nested": obj(t, map[string]attr.Value{
			"endpoint": types.StringValue("db.example.com:5432"),
		}),
	}))
	got, err := FromValue(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"alpha":true,"list":["a<b>&",0.5],"nested":{"endpoint":"db.example.com:5432"},"zeta":5432}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestUnknownIsAnError(t *testing.T) {
	v := types.DynamicValue(obj(t, map[string]attr.Value{"endpoint": types.StringUnknown()}))
	if IsFullyKnown(v) {
		t.Fatal("an unknown attribute makes the value not fully known")
	}
	if _, err := FromValue(context.Background(), v); err == nil {
		t.Fatal("encoding an unknown value must fail")
	}
	if !IsFullyKnown(types.DynamicNull()) || IsFullyKnown(types.DynamicUnknown()) {
		t.Fatal("null is known; unknown isn't")
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, in := range []string{
		`{"postgres":{"endpoint":"orders.example.com:5432","iam_auth":true,"port":5432}}`,
		`[{"idp_group":"okta-payments-eng","roles":["dev-readonly"]},{"idp_group":"x","roles":[]}]`,
		`{"big":12345678901234567890,"cases":[{"name":"n","who":{"groups":["developers"]}}],"ratio":0.25}`,
		`"just a string"`,
		`{}`,
		`[]`,
	} {
		v, err := ToValue([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		out, err := FromValue(ctx, v)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if string(out) != in {
			t.Fatalf("round trip changed the data:\n in  %s\n out %s", in, out)
		}
	}
}

func TestNullsAreUnset(t *testing.T) {
	v, err := ToValue([]byte(`{"a":null,"b":1}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := FromValue(context.Background(), v)
	if string(out) != `{"b":1}` {
		t.Fatalf("got %s", out)
	}
	if v, _ := ToValue([]byte("null")); !v.IsNull() {
		t.Fatal("JSON null is a null value")
	}
}

func TestEqual(t *testing.T) {
	for _, c := range []struct {
		a, b string
		eq   bool
	}{
		{`{"b":1,"a":[1,2]}`, `{ "a": [1, 2], "b": 1.0 }`, true},
		{`{"a":1e3}`, `{"a":1000}`, true},
		{`{"a":null,"b":1}`, `{"b":1}`, true},
		{`{"a":[1,2]}`, `{"a":[2,1]}`, false},
		{`{"a":"1"}`, `{"a":1}`, false},
		{`{"a":0.1}`, `{"a":0.10000000000000001}`, false},
		{`not json`, `not json`, false},
	} {
		if got := Equal([]byte(c.a), []byte(c.b)); got != c.eq {
			t.Errorf("Equal(%s, %s) = %v, want %v", c.a, c.b, got, c.eq)
		}
	}
}

func TestCanonicalRejectsTrailingData(t *testing.T) {
	if _, err := Canonical([]byte(`{} {}`)); err == nil {
		t.Fatal("two values must be refused")
	}
}
