package mcp

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Param is a path or query parameter of a tool.
type Param struct {
	Name     string
	In       string // path or query
	Required bool
}

// Body describes the request body of a tool.
type Body struct {
	// Flat: the body properties are top-level tool arguments (Props).
	// Otherwise the whole body is the "body" argument.
	Flat     bool
	Props    []string
	Required bool
}

// toolPlan is a tool with what dispatch needs.
type toolPlan struct {
	Tool
	Params []Param
	Body   *Body
}

// Tools returns the MCP tools of the operations with x-mcp-tool in an
// OpenAPI 3.0 document, sorted by name.
func Tools(spec []byte) ([]Tool, error) {
	plans, err := plan(spec)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Tool)
	}
	return out, nil
}

var methods = []string{"get", "put", "post", "delete", "patch"}

func plan(spec []byte) ([]toolPlan, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	r := &resolver{doc: doc}
	paths, _ := doc["paths"].(map[string]any)
	var out []toolPlan
	seen := map[string]string{}
	for path, item := range paths {
		pi, _ := item.(map[string]any)
		for _, m := range methods {
			op, ok := pi[m].(map[string]any)
			if !ok {
				continue
			}
			name, _ := op["x-mcp-tool"].(string)
			if name == "" {
				continue
			}
			if other, dup := seen[name]; dup {
				return nil, fmt.Errorf("openapi: tool %s on %s and %s", name, other, path)
			}
			seen[name] = path
			tp, err := r.toolOf(name, strings.ToUpper(m), path, op, pi)
			if err != nil {
				return nil, fmt.Errorf("openapi: %s: %w", name, err)
			}
			out = append(out, tp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type resolver struct {
	doc map[string]any
}

// ref returns the object a local $ref points to.
func (r *resolver) ref(ref string) (map[string]any, bool) {
	p, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	var cur any = r.doc
	for _, part := range strings.Split(p, "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = m[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
	}
	m, ok := cur.(map[string]any)
	return m, ok
}

func (r *resolver) toolOf(name, method, path string, op, item map[string]any) (toolPlan, error) {
	tp := toolPlan{Tool: Tool{Name: name, Method: method, Path: path}}
	tp.OperationID, _ = op["operationId"].(string)
	summary, _ := op["summary"].(string)
	desc, _ := op["description"].(string)
	tp.Description = strings.TrimSpace(strings.Join(nonEmpty(summary, desc), "\n\n"))
	if tp.Description == "" {
		tp.Description = method + " " + path
	}
	props := map[string]any{}
	var required []string

	var params []any
	if ps, ok := item["parameters"].([]any); ok {
		params = append(params, ps...)
	}
	if ps, ok := op["parameters"].([]any); ok {
		params = append(params, ps...)
	}
	for _, raw := range params {
		p, _ := raw.(map[string]any)
		if ref, ok := p["$ref"].(string); ok {
			if p, ok = r.ref(ref); !ok {
				return tp, fmt.Errorf("bad ref %s", ref)
			}
		}
		pname, _ := p["name"].(string)
		in, _ := p["in"].(string)
		if pname == "" || (in != "path" && in != "query") {
			continue
		}
		req, _ := p["required"].(bool)
		if in == "path" {
			req = true
		}
		sch, _ := p["schema"].(map[string]any)
		s := r.schema(sch, map[string]bool{}, 0)
		if d, ok := p["description"].(string); ok && d != "" {
			if _, has := s["description"]; !has {
				s["description"] = d
			}
		}
		props[pname] = s
		if req {
			required = append(required, pname)
		}
		tp.Params = append(tp.Params, Param{Name: pname, In: in, Required: req})
	}

	if rb, ok := op["requestBody"].(map[string]any); ok {
		if ref, ok := rb["$ref"].(string); ok {
			rb, _ = r.ref(ref)
		}
		breq, _ := rb["required"].(bool)
		content, _ := rb["content"].(map[string]any)
		mt, _ := content["application/json"].(map[string]any)
		sch, _ := mt["schema"].(map[string]any)
		bs := r.schema(sch, map[string]bool{}, 0)
		body := &Body{Required: breq}
		bprops, _ := bs["properties"].(map[string]any)
		flat := len(bprops) > 0
		for k := range bprops {
			if _, clash := props[k]; clash {
				flat = false
			}
		}
		if flat {
			body.Flat = true
			for k, v := range bprops {
				props[k] = v
				body.Props = append(body.Props, k)
			}
			sort.Strings(body.Props)
			if breq {
				if req, ok := bs["required"].([]any); ok {
					for _, x := range req {
						if s, ok := x.(string); ok {
							required = append(required, s)
						}
					}
				}
			}
		} else {
			props["body"] = bs
			if breq {
				required = append(required, "body")
			}
		}
		tp.Body = body
	}
	tp.InputSchema = map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		tp.InputSchema["required"] = required
	}
	return tp, nil
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// schema converts an OpenAPI 3.0 schema to JSON Schema with refs inlined.
// Recursive refs and deep nesting end in a plain object.
func (r *resolver) schema(s map[string]any, stack map[string]bool, depth int) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	if ref, ok := s["$ref"].(string); ok {
		if stack[ref] || depth > 8 {
			return map[string]any{"type": "object"}
		}
		target, ok := r.ref(ref)
		if !ok {
			return map[string]any{}
		}
		stack[ref] = true
		out := r.schema(target, stack, depth+1)
		delete(stack, ref)
		return out
	}
	out := map[string]any{}
	nullable, _ := s["nullable"].(bool)
	for k, v := range s {
		switch {
		case k == "nullable", k == "example", k == "xml", k == "discriminator", k == "readOnly", k == "writeOnly", k == "deprecated":
			continue
		case strings.HasPrefix(k, "x-"):
			continue
		}
		switch k {
		case "properties":
			m, _ := v.(map[string]any)
			pm := map[string]any{}
			for pk, pv := range m {
				ps, _ := pv.(map[string]any)
				pm[pk] = r.schema(ps, stack, depth+1)
			}
			out[k] = pm
		case "items", "not":
			m, _ := v.(map[string]any)
			out[k] = r.schema(m, stack, depth+1)
		case "additionalProperties":
			if m, ok := v.(map[string]any); ok {
				out[k] = r.schema(m, stack, depth+1)
			} else {
				out[k] = v
			}
		case "allOf", "anyOf", "oneOf":
			l, _ := v.([]any)
			var list []any
			for _, x := range l {
				m, _ := x.(map[string]any)
				list = append(list, r.schema(m, stack, depth+1))
			}
			if k == "allOf" {
				// Merge allOf into one object schema (clients handle it better).
				merged := mergeAllOf(list)
				for mk, mv := range merged {
					out[mk] = mv
				}
				continue
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	if nullable {
		if t, ok := out["type"].(string); ok {
			out["type"] = []any{t, "null"}
		}
	}
	return out
}

func mergeAllOf(list []any) map[string]any {
	out := map[string]any{}
	props := map[string]any{}
	var required []any
	for _, x := range list {
		m, _ := x.(map[string]any)
		for k, v := range m {
			switch k {
			case "properties":
				pm, _ := v.(map[string]any)
				for pk, pv := range pm {
					props[pk] = pv
				}
			case "required":
				l, _ := v.([]any)
				required = append(required, l...)
			default:
				out[k] = v
			}
		}
	}
	if len(props) > 0 {
		out["properties"] = props
		out["type"] = "object"
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
