package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdDoc is one kind: its names, its printer columns, and its fields as flat
// rows in document order, so a page renders a field table without walking a
// schema.
type crdDoc struct {
	Kind              string          `json:"kind"`
	ListKind          string          `json:"listKind"`
	Group             string          `json:"group"`
	Version           string          `json:"version"`
	Plural            string          `json:"plural"`
	Singular          string          `json:"singular"`
	ShortNames        []string        `json:"shortNames"`
	Categories        []string        `json:"categories"`
	Scope             string          `json:"scope"`
	Source            string          `json:"source"`
	Description       string          `json:"description"`
	StatusSubresource bool            `json:"statusSubresource"`
	PrinterColumns    []printerColumn `json:"printerColumns"`
	// Validations are the CEL rules on the object itself (x-kubernetes-validations
	// on the root schema); rules on a field are on its row.
	Validations []celRule  `json:"validations"`
	Fields      []fieldRow `json:"fields"`
}

type printerColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	JSONPath    string `json:"jsonPath"`
	Priority    int32  `json:"priority"`
	Description string `json:"description,omitempty"`
}

type celRule struct {
	Rule              string `json:"rule"`
	Message           string `json:"message,omitempty"`
	MessageExpression string `json:"messageExpression,omitempty"`
	Reason            string `json:"reason,omitempty"`
	FieldPath         string `json:"fieldPath,omitempty"`
	OptionalOldSelf   bool   `json:"optionalOldSelf,omitempty"`
}

// fieldRow is one field. Path is the dotted path from the object root and is
// also the anchor on the kind's page: "spec.bmc.address". An array's element
// adds "[]" ("spec.actions[].powerCycle"), a map's value adds "{}".
type fieldRow struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Parent      string    `json:"parent"`
	Depth       int       `json:"depth"`
	Type        string    `json:"type"`
	Format      string    `json:"format,omitempty"`
	Required    bool      `json:"required"`
	Default     any       `json:"default,omitempty"`
	Enum        []any     `json:"enum,omitempty"`
	Pattern     string    `json:"pattern,omitempty"`
	Minimum     *float64  `json:"minimum,omitempty"`
	Maximum     *float64  `json:"maximum,omitempty"`
	MinLength   *int64    `json:"minLength,omitempty"`
	MaxLength   *int64    `json:"maxLength,omitempty"`
	MinItems    *int64    `json:"minItems,omitempty"`
	MaxItems    *int64    `json:"maxItems,omitempty"`
	MinProps    *int64    `json:"minProperties,omitempty"`
	MaxProps    *int64    `json:"maxProperties,omitempty"`
	ListType    string    `json:"listType,omitempty"`
	ListMapKeys []string  `json:"listMapKeys,omitempty"`
	MapType     string    `json:"mapType,omitempty"`
	Nullable    bool      `json:"nullable,omitempty"`
	Unknown     bool      `json:"preserveUnknownFields,omitempty"`
	Description string    `json:"description"`
	Validations []celRule `json:"validations,omitempty"`
}

func readCRDs(cfg config) ([]crdDoc, error) {
	paths, err := filepath.Glob(filepath.Join(cfg.crdDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var docs []crdDoc
	for _, p := range paths {
		if filepath.Base(p) == "kustomization.yaml" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var crd apiextv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}
		doc, err := convertCRD(&crd)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		doc.Source = relPath(cfg.root, p)
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no CRDs in %s", cfg.crdDir)
	}
	slices.SortFunc(docs, func(a, b crdDoc) int { return strings.Compare(a.Kind, b.Kind) })
	return docs, nil
}

func convertCRD(crd *apiextv1.CustomResourceDefinition) (crdDoc, error) {
	// The site has one page per kind. A second version needs a decision on
	// how pages show it, so refuse it here rather than drop it silently.
	if len(crd.Spec.Versions) != 1 {
		return crdDoc{}, fmt.Errorf("%d versions; docgen renders exactly one", len(crd.Spec.Versions))
	}
	v := crd.Spec.Versions[0]
	if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
		return crdDoc{}, fmt.Errorf("version %s has no schema", v.Name)
	}
	root := v.Schema.OpenAPIV3Schema
	n := crd.Spec.Names
	doc := crdDoc{
		Kind:              n.Kind,
		ListKind:          n.ListKind,
		Group:             crd.Spec.Group,
		Version:           v.Name,
		Plural:            n.Plural,
		Singular:          n.Singular,
		ShortNames:        nonNil(n.ShortNames),
		Categories:        nonNil(n.Categories),
		Scope:             string(crd.Spec.Scope),
		Description:       root.Description,
		StatusSubresource: v.Subresources != nil && v.Subresources.Status != nil,
		PrinterColumns:    []printerColumn{},
		Validations:       celRules(root.XValidations),
	}
	for _, c := range v.AdditionalPrinterColumns {
		doc.PrinterColumns = append(doc.PrinterColumns, printerColumn{
			Name: c.Name, Type: c.Type, JSONPath: c.JSONPath, Priority: c.Priority, Description: c.Description,
		})
	}
	var rows []fieldRow
	for _, name := range sortedKeys(root.Properties) {
		// apiVersion and kind carry the same Kubernetes boilerplate on every
		// kind; metadata matters only when the kind constrains it.
		if name == "apiVersion" || name == "kind" {
			continue
		}
		prop := root.Properties[name]
		if name == "metadata" && len(prop.Properties) == 0 && len(prop.XValidations) == 0 {
			continue
		}
		var err error
		rows, err = walkField(rows, name, "", name, 0, slices.Contains(root.Required, name), &prop)
		if err != nil {
			return crdDoc{}, err
		}
	}
	doc.Fields = nonNil(rows)
	return doc, nil
}

// walkField appends the row for s at path and then the rows below it.
func walkField(rows []fieldRow, path, parent, name string, depth int, required bool, s *apiextv1.JSONSchemaProps) ([]fieldRow, error) {
	row := fieldRow{
		Path:        path,
		Name:        name,
		Parent:      parent,
		Depth:       depth,
		Type:        typeOf(s),
		Format:      s.Format,
		Required:    required,
		Pattern:     s.Pattern,
		Minimum:     s.Minimum,
		Maximum:     s.Maximum,
		MinLength:   s.MinLength,
		MaxLength:   s.MaxLength,
		MinItems:    s.MinItems,
		MaxItems:    s.MaxItems,
		MinProps:    s.MinProperties,
		MaxProps:    s.MaxProperties,
		ListMapKeys: s.XListMapKeys,
		Nullable:    s.Nullable,
		Unknown:     s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields,
		Description: fieldDescription(parent, name, s.Description),
		Validations: celRules(s.XValidations),
	}
	if s.XListType != nil {
		row.ListType = *s.XListType
	}
	if s.XMapType != nil {
		row.MapType = *s.XMapType
	}
	if s.Default != nil {
		v, err := decodeJSON(s.Default.Raw)
		if err != nil {
			return nil, fmt.Errorf("%s: default: %w", path, err)
		}
		row.Default = v
	}
	for _, e := range s.Enum {
		v, err := decodeJSON(e.Raw)
		if err != nil {
			return nil, fmt.Errorf("%s: enum: %w", path, err)
		}
		row.Enum = append(row.Enum, v)
	}
	// An array's element schema and a map's value schema carry their own
	// bounds and rules (an enum on []string sits on the element); show them on
	// the field's row, where a reader looks for them.
	switch elem := elemSchema(s); {
	case elem != nil && !hasChildren(elem):
		if row.Pattern == "" {
			row.Pattern = elem.Pattern
		}
		if len(row.Enum) == 0 {
			for _, e := range elem.Enum {
				v, err := decodeJSON(e.Raw)
				if err != nil {
					return nil, fmt.Errorf("%s: enum: %w", path, err)
				}
				row.Enum = append(row.Enum, v)
			}
		}
		if row.Format == "" {
			row.Format = elem.Format
		}
		row.Validations = append(row.Validations, celRules(elem.XValidations)...)
	}
	rows = append(rows, row)

	if s.Items != nil && s.Items.Schema != nil {
		return walkChildren(rows, path+"[]", path, depth, s.Items.Schema)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
		return walkChildren(rows, path+"{}", path, depth, s.AdditionalProperties.Schema)
	}
	return walkChildren(rows, path, path, depth, s)
}

// walkChildren appends the rows for the properties of s; prefix is the path
// the children hang off ("spec.actions[]" for the elements of an array).
func walkChildren(rows []fieldRow, prefix, parent string, depth int, s *apiextv1.JSONSchemaProps) ([]fieldRow, error) {
	for _, name := range sortedKeys(s.Properties) {
		prop := s.Properties[name]
		var err error
		rows, err = walkField(rows, prefix+"."+name, parent, name, depth+1, slices.Contains(s.Required, name), &prop)
		if err != nil {
			return nil, err
		}
	}
	// Element rules of an object array sit on the element, which has no row
	// of its own: attach them to the array's row.
	if prefix != parent && hasChildren(s) && len(s.XValidations) > 0 {
		for i := range rows {
			if rows[i].Path == parent {
				rows[i].Validations = append(rows[i].Validations, celRules(s.XValidations)...)
			}
		}
	}
	return rows, nil
}

func elemSchema(s *apiextv1.JSONSchemaProps) *apiextv1.JSONSchemaProps {
	if s.Items != nil && s.Items.Schema != nil {
		return s.Items.Schema
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
		return s.AdditionalProperties.Schema
	}
	return nil
}

func hasChildren(s *apiextv1.JSONSchemaProps) bool {
	return len(s.Properties) > 0 || elemSchema(s) != nil
}

// typeOf names a schema's type the way a Go reader expects it: "[]string",
// "map[string]string", "int-or-string".
func typeOf(s *apiextv1.JSONSchemaProps) string {
	switch {
	case s.XIntOrString:
		return "int-or-string"
	case s.Type == "array":
		if s.Items != nil && s.Items.Schema != nil {
			return "[]" + typeOf(s.Items.Schema)
		}
		return "[]any"
	case s.Type == "object" && s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil:
		return "map[string]" + typeOf(s.AdditionalProperties.Schema)
	case s.Type == "":
		return "any"
	}
	return s.Type
}

func celRules(vs apiextv1.ValidationRules) []celRule {
	var out []celRule
	for _, v := range vs {
		r := celRule{Rule: v.Rule, Message: v.Message, MessageExpression: v.MessageExpression, FieldPath: v.FieldPath}
		if v.Reason != nil {
			r.Reason = string(*v.Reason)
		}
		if v.OptionalOldSelf != nil {
			r.OptionalOldSelf = *v.OptionalOldSelf
		}
		out = append(out, r)
	}
	if out == nil {
		return []celRule{}
	}
	return out
}

func decodeJSON(raw []byte) (any, error) {
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// nonNil turns a nil slice into an empty one, so the JSON has [] and the site
// can iterate without a null check.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
