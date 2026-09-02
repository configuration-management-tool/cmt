// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package manifest

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// attrString reads a string-valued attribute. ok is false when the
// attribute is absent.
func attrString(attrs hclsyntax.Attributes, name string) (value string, ok bool, err error) {
	a, present := attrs[name]
	if !present {
		return "", false, nil
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() {
		return "", true, fmt.Errorf("%s: %s", a.SrcRange, diags)
	}
	sv, convErr := convert.Convert(v, cty.String)
	if convErr != nil {
		return "", true, fmt.Errorf("%s: %q must be a string: %w", a.SrcRange, name, convErr)
	}
	if sv.IsNull() {
		return "", true, fmt.Errorf("%s: %q must not be null", a.SrcRange, name)
	}
	return sv.AsString(), true, nil
}

// attrBool reads a bool-valued attribute. ok is false when the attribute
// is absent.
func attrBool(attrs hclsyntax.Attributes, name string) (value bool, ok bool, err error) {
	a, present := attrs[name]
	if !present {
		return false, false, nil
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() {
		return false, true, fmt.Errorf("%s: %s", a.SrcRange, diags)
	}
	bv, convErr := convert.Convert(v, cty.Bool)
	if convErr != nil {
		return false, true, fmt.Errorf("%s: %q must be a bool: %w", a.SrcRange, name, convErr)
	}
	if bv.IsNull() {
		return false, true, fmt.Errorf("%s: %q must not be null", a.SrcRange, name)
	}
	return bv.True(), true, nil
}

// attrInt reads a number-valued attribute, truncating toward zero. ok is
// false when the attribute is absent.
func attrInt(attrs hclsyntax.Attributes, name string) (value int, ok bool, err error) {
	a, present := attrs[name]
	if !present {
		return 0, false, nil
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() {
		return 0, true, fmt.Errorf("%s: %s", a.SrcRange, diags)
	}
	nv, convErr := convert.Convert(v, cty.Number)
	if convErr != nil {
		return 0, true, fmt.Errorf("%s: %q must be a number: %w", a.SrcRange, name, convErr)
	}
	if nv.IsNull() {
		return 0, true, fmt.Errorf("%s: %q must not be null", a.SrcRange, name)
	}
	i64, _ := nv.AsBigFloat().Int64()
	return int(i64), true, nil
}

// attrStringList reads a list/tuple/set-of-string attribute. ok is false
// when the attribute is absent.
func attrStringList(attrs hclsyntax.Attributes, name string) (value []string, ok bool, err error) {
	a, present := attrs[name]
	if !present {
		return nil, false, nil
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() {
		return nil, true, fmt.Errorf("%s: %s", a.SrcRange, diags)
	}
	if v.IsNull() {
		return nil, true, fmt.Errorf("%s: %q must not be null", a.SrcRange, name)
	}
	if !v.CanIterateElements() {
		return nil, true, fmt.Errorf("%s: %q must be a list of strings", a.SrcRange, name)
	}
	out := []string{}
	it := v.ElementIterator()
	for it.Next() {
		_, ev := it.Element()
		sv, convErr := convert.Convert(ev, cty.String)
		if convErr != nil {
			return nil, true, fmt.Errorf("%s: %q must be a list of strings: %w", a.SrcRange, name, convErr)
		}
		out = append(out, sv.AsString())
	}
	return out, true, nil
}

// attrStringMap reads an object-valued attribute (`env = { K = "v" }`,
// the only shape cmt's manifest schema ever produces for this kind of
// field — HCL2 native-syntax object constructors evaluate to an object
// type, not a map type) as map[string]string. ok is false when the
// attribute is absent.
func attrStringMap(attrs hclsyntax.Attributes, name string) (value map[string]string, ok bool, err error) {
	a, present := attrs[name]
	if !present {
		return nil, false, nil
	}
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() {
		return nil, true, fmt.Errorf("%s: %s", a.SrcRange, diags)
	}
	if v.IsNull() {
		return nil, true, fmt.Errorf("%s: %q must not be null", a.SrcRange, name)
	}
	if !v.Type().IsObjectType() {
		return nil, true, fmt.Errorf("%s: %q must be a map, e.g. { KEY = \"value\" }", a.SrcRange, name)
	}
	out := map[string]string{}
	for k := range v.Type().AttributeTypes() {
		sv, convErr := convert.Convert(v.GetAttr(k), cty.String)
		if convErr != nil {
			return nil, true, fmt.Errorf("%s: %q.%s must be a string: %w", a.SrcRange, name, k, convErr)
		}
		out[k] = sv.AsString()
	}
	return out, true, nil
}

// attrsToStringMap converts every attribute in attrs (e.g. an env{}
// block's body) into map[string]string.
func attrsToStringMap(attrs hclsyntax.Attributes) (map[string]string, error) {
	out := map[string]string{}
	for name, a := range attrs {
		v, diags := a.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s: %s", a.SrcRange, diags)
		}
		sv, convErr := convert.Convert(v, cty.String)
		if convErr != nil {
			return nil, fmt.Errorf("%s: env value %q must be a string: %w", a.SrcRange, name, convErr)
		}
		if sv.IsNull() {
			return nil, fmt.Errorf("%s: env value %q must not be null", a.SrcRange, name)
		}
		out[name] = sv.AsString()
	}
	return out, nil
}

// checkUnknownAttrs returns an error naming the first (alphabetically)
// attribute in attrs that is not in allowed.
func checkUnknownAttrs(attrs hclsyntax.Attributes, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	var bad []string
	for name := range attrs {
		if !set[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	a := attrs[bad[0]]
	return fmt.Errorf("%s: unexpected attribute %q", a.SrcRange, bad[0])
}

// checkUnknownBlocks returns an error naming the first block in blocks
// whose type is not in allowed.
func checkUnknownBlocks(blocks []*hclsyntax.Block, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	for _, b := range blocks {
		if !set[b.Type] {
			return fmt.Errorf("%s: unexpected block %q", b.TypeRange, b.Type)
		}
	}
	return nil
}

// soleBlock returns the single block of the given type among blocks, or
// nil if none is present. It errors if there is more than one, or if the
// block carries labels.
func soleBlock(blocks []*hclsyntax.Block, blockType string) (*hclsyntax.Block, error) {
	var found *hclsyntax.Block
	for _, b := range blocks {
		if b.Type != blockType {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%s: duplicate %q block", b.TypeRange, blockType)
		}
		if len(b.Labels) != 0 {
			return nil, fmt.Errorf("%s: %q block takes no label", b.TypeRange, blockType)
		}
		found = b
	}
	return found, nil
}
