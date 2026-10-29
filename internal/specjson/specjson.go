// Package specjson converts a document's `spec` between Terraform values and
// JSON, and compares specs the way Limen does: as data, not as text.
//
// A spec in Terraform is a dynamic value (any object, list, or scalar, in
// exactly the YAML shape). Limen stores it as JSON and returns it normalized:
// keys sorted and defaults left out.
package specjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

// FromValue encodes a Terraform value as canonical JSON. Null attributes of
// an object are left out, because in Terraform null means "not set".
// Unknown values are an error: call it only on known values.
func FromValue(ctx context.Context, v attr.Value) ([]byte, error) {
	data, err := toData(ctx, v, "spec")
	if err != nil {
		return nil, err
	}
	return marshal(data)
}

// IsFullyKnown reports whether v and everything inside it is known.
func IsFullyKnown(v attr.Value) bool {
	if v == nil || v.IsUnknown() {
		return false
	}
	if v.IsNull() {
		return true
	}
	switch v := v.(type) {
	case basetypes.DynamicValue:
		return !v.IsUnderlyingValueUnknown() && IsFullyKnown(v.UnderlyingValue())
	case basetypes.ObjectValue:
		for _, a := range v.Attributes() {
			if !IsFullyKnown(a) {
				return false
			}
		}
	case basetypes.MapValue:
		for _, a := range v.Elements() {
			if !IsFullyKnown(a) {
				return false
			}
		}
	case basetypes.ListValue:
		return allKnown(v.Elements())
	case basetypes.SetValue:
		return allKnown(v.Elements())
	case basetypes.TupleValue:
		return allKnown(v.Elements())
	}
	return true
}

func allKnown(vs []attr.Value) bool {
	for _, e := range vs {
		if !IsFullyKnown(e) {
			return false
		}
	}
	return true
}

func toData(ctx context.Context, v attr.Value, path string) (any, error) {
	if v == nil || v.IsNull() {
		return nil, nil
	}
	if v.IsUnknown() {
		return nil, fmt.Errorf("%s is not known yet", path)
	}
	switch v := v.(type) {
	case basetypes.DynamicValue:
		if v.IsUnderlyingValueNull() {
			return nil, nil
		}
		if v.IsUnderlyingValueUnknown() {
			return nil, fmt.Errorf("%s is not known yet", path)
		}
		return toData(ctx, v.UnderlyingValue(), path)
	case basetypes.StringValue:
		return v.ValueString(), nil
	case basetypes.BoolValue:
		return v.ValueBool(), nil
	case basetypes.NumberValue:
		return json.Number(formatNumber(v.ValueBigFloat())), nil
	case basetypes.Int64Value:
		return json.Number(fmt.Sprint(v.ValueInt64())), nil
	case basetypes.Int32Value:
		return json.Number(fmt.Sprint(v.ValueInt32())), nil
	case basetypes.Float64Value:
		return json.Number(formatNumber(big.NewFloat(v.ValueFloat64()))), nil
	case basetypes.Float32Value:
		return json.Number(formatNumber(big.NewFloat(float64(v.ValueFloat32())))), nil
	case basetypes.ObjectValue:
		return objectData(ctx, v.Attributes(), path)
	case basetypes.MapValue:
		return objectData(ctx, v.Elements(), path)
	case basetypes.ListValue:
		return listData(ctx, v.Elements(), path)
	case basetypes.SetValue:
		return listData(ctx, v.Elements(), path)
	case basetypes.TupleValue:
		return listData(ctx, v.Elements(), path)
	}
	return nil, fmt.Errorf("%s: values of type %s can't be part of a spec", path, v.Type(ctx))
}

func objectData(ctx context.Context, attrs map[string]attr.Value, path string) (any, error) {
	out := make(map[string]any, len(attrs))
	for k, a := range attrs {
		d, err := toData(ctx, a, path+"."+k)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out[k] = d
		}
	}
	return out, nil
}

func listData(ctx context.Context, elems []attr.Value, path string) (any, error) {
	out := make([]any, 0, len(elems))
	for i, e := range elems {
		d, err := toData(ctx, e, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// ToValue decodes JSON into a Terraform dynamic value: objects become
// objects, arrays tuples, numbers numbers. A JSON null becomes a null value.
func ToValue(data []byte) (types.Dynamic, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return types.DynamicNull(), nil
	}
	parsed, err := decode(data)
	if err != nil {
		return types.DynamicNull(), err
	}
	v, err := fromData(parsed)
	if err != nil {
		return types.DynamicNull(), err
	}
	if v == nil {
		return types.DynamicNull(), nil
	}
	return types.DynamicValue(v), nil
}

func fromData(d any) (attr.Value, error) {
	switch d := d.(type) {
	case nil:
		return nil, nil
	case string:
		return types.StringValue(d), nil
	case bool:
		return types.BoolValue(d), nil
	case json.Number:
		f, _, err := big.ParseFloat(string(d), 10, 512, big.ToNearestEven)
		if err != nil {
			return nil, fmt.Errorf("number %s: %w", d, err)
		}
		return types.NumberValue(f), nil
	case map[string]any:
		attrTypes := make(map[string]attr.Type, len(d))
		attrs := make(map[string]attr.Value, len(d))
		for k, e := range d {
			v, err := fromData(e)
			if err != nil {
				return nil, err
			}
			if v == nil {
				// A JSON null is the same as the key not being there.
				continue
			}
			attrTypes[k], attrs[k] = v.Type(context.Background()), v
		}
		obj, diags := types.ObjectValue(attrTypes, attrs)
		if diags.HasError() {
			return nil, fmt.Errorf("building an object: %v", diags)
		}
		return obj, nil
	case []any:
		elemTypes := make([]attr.Type, 0, len(d))
		elems := make([]attr.Value, 0, len(d))
		for _, e := range d {
			v, err := fromData(e)
			if err != nil {
				return nil, err
			}
			if v == nil {
				v = types.DynamicNull()
			}
			elemTypes, elems = append(elemTypes, v.Type(context.Background())), append(elems, v)
		}
		tup, diags := types.TupleValue(elemTypes, elems)
		if diags.HasError() {
			return nil, fmt.Errorf("building a tuple: %v", diags)
		}
		return tup, nil
	}
	return nil, fmt.Errorf("unexpected JSON value %T", d)
}

// Canonical re-encodes JSON with sorted keys, no insignificant space,
// numbers in their shortest exact form, and null object members left out,
// so two encodings of the same data are byte-equal.
func Canonical(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return []byte("null"), nil
	}
	parsed, err := decode(data)
	if err != nil {
		return nil, err
	}
	return marshal(dropNulls(parsed))
}

// Equal reports whether two JSON documents hold the same data. Invalid JSON
// is equal to nothing.
func Equal(a, b []byte) bool {
	ca, err := Canonical(a)
	if err != nil {
		return false
	}
	cb, err := Canonical(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid JSON: more than one value")
	}
	return normalizeNumbers(v)
}

func normalizeNumbers(v any) (any, error) {
	switch v := v.(type) {
	case json.Number:
		f, _, err := big.ParseFloat(string(v), 10, 512, big.ToNearestEven)
		if err != nil {
			return nil, fmt.Errorf("invalid number %s", v)
		}
		return json.Number(formatNumber(f)), nil
	case map[string]any:
		for k, e := range v {
			n, err := normalizeNumbers(e)
			if err != nil {
				return nil, err
			}
			v[k] = n
		}
	case []any:
		for i, e := range v {
			n, err := normalizeNumbers(e)
			if err != nil {
				return nil, err
			}
			v[i] = n
		}
	}
	return v, nil
}

func dropNulls(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if e == nil {
				delete(v, k)
				continue
			}
			v[k] = dropNulls(e)
		}
	case []any:
		for i, e := range v {
			v[i] = dropNulls(e)
		}
	}
	return v
}

// formatNumber writes f exactly, as an integer when it is one.
func formatNumber(f *big.Float) string {
	if f.IsInt() {
		i, _ := f.Int(nil)
		return i.String()
	}
	s := f.Text('g', -1)
	// big.Float's shortest form can use an exponent; JSON allows it, but
	// keep the common case plain.
	if !strings.ContainsAny(s, "eE") {
		return s
	}
	return f.Text('f', -1)
}

// marshal encodes with sorted keys and without HTML escaping.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := write(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func write(buf *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		buf.WriteString("null")
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeScalar(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := write(buf, v[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return writeScalar(buf, v)
	}
	return nil
}

func writeScalar(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode adds a newline
	return nil
}
