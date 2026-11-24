package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// goSources is what docgen reads from the Go code rather than from a manifest:
// the action union of HostOperation, the typed string constants (phases and
// other enums), the condition and reason constants, and the label, annotation
// and finalizer keys.
type goSources struct {
	actions    actionsDoc
	phases     phasesDoc
	enums      enumsDoc
	conditions conditionsDoc
	keys       keysDoc
}

type actionsDoc struct {
	Kind    string   `json:"kind"`
	Source  string   `json:"source"`
	Actions []action `json:"actions"`
}

// action is one member of the HostOperationAction union, in field order.
type action struct {
	// Key is the JSON name ("powerCycle"): the step name in status and in
	// pk-metal output, and the anchor on the actions page.
	Key             string `json:"key"`
	GoName          string `json:"goName"`
	Type            string `json:"type"`
	Path            string `json:"path"`
	Description     string `json:"description"`
	TypeDescription string `json:"typeDescription"`
	// NodeRef is true when the action names a TalosNode: it acts through the
	// node's Talos or Kubernetes API, or waits for the node, not the BMC.
	NodeRef bool          `json:"nodeRef"`
	Fields  []actionField `json:"fields"`
}

// actionField is a field of an action. Type and Required come from the CRD
// row at the action's path, which is what the API server enforces; GoType is
// the field's type in the Go source.
type actionField struct {
	Name        string   `json:"name"`
	GoName      string   `json:"goName"`
	Type        string   `json:"type"`
	GoType      string   `json:"goType"`
	Required    bool     `json:"required"`
	Description string   `json:"description"`
	Markers     []string `json:"markers"`
}

type enumsDoc struct {
	Enums []enumDoc `json:"enums"`
}

// phasesDoc holds the enums that are a kind's status.phase.
type phasesDoc struct {
	Phases []enumDoc `json:"phases"`
}

// enumDoc is a typed string with its constants. For a phase, Kinds names the
// kinds whose status.phase it is.
type enumDoc struct {
	Type        string      `json:"type"`
	Group       string      `json:"group"`
	Source      string      `json:"source"`
	Description string      `json:"description"`
	Kinds       []string    `json:"kinds"`
	UsedBy      []string    `json:"usedBy"`
	Values      []enumValue `json:"values"`
}

type enumValue struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description"`
}

type conditionsDoc struct {
	Conditions       []constDoc  `json:"conditions"`
	Reasons          []reasonDoc `json:"reasons"`
	OperationReasons []constDoc  `json:"operationReasons"`
}

type constDoc struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Group       string `json:"group"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

// reasonDoc is a condition reason. On lists the condition=status pairs its
// comment names ("on Executed=Unknown"); Context is the comment of the const
// block it is declared in ("Reasons on the claim's Provisioned condition").
type reasonDoc struct {
	constDoc
	On      []condRef `json:"on"`
	Context string    `json:"context"`
}

type condRef struct {
	Condition string `json:"condition"`
	Status    string `json:"status"`
}

type keysDoc struct {
	Keys []keyDoc `json:"keys"`
}

// keyDoc is a label, annotation or finalizer key under metal.pk.io/ or
// os.pk.io/, with every constant that spells it.
type keyDoc struct {
	Key         string     `json:"key"`
	Kind        string     `json:"kind"`
	Exported    bool       `json:"exported"`
	Description string     `json:"description"`
	Constants   []keyConst `json:"constants"`
}

type keyConst struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// goConst is a string constant as declared.
type goConst struct {
	name, typ, value, doc, blockDoc, source string
}

// goType is a named type with its comment, and its fields when it is a struct.
type goType struct {
	name, doc, source string
	st                *ast.StructType
}

type goPackage struct {
	group  string
	consts []goConst
	types  map[string]goType
}

func readGoSources(cfg config) (goSources, error) {
	var src goSources
	var pkgs []goPackage
	for _, dir := range cfg.apiDirs {
		p, err := parsePackage(cfg.root, dir)
		if err != nil {
			return src, err
		}
		pkgs = append(pkgs, p)
	}
	var err error
	if src.actions, err = buildActions(pkgs); err != nil {
		return src, err
	}
	src.enums, src.phases = buildEnums(pkgs)
	src.conditions = buildConditions(pkgs)
	if src.keys, err = scanKeys(cfg); err != nil {
		return src, err
	}
	return src, nil
}

func parsePackage(root, dir string) (goPackage, error) {
	p := goPackage{types: map[string]goType{}}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return p, err
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if !isSourceFile(path) {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return p, err
		}
		source := relPath(root, path)
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if _, g, ok := strings.Cut(c.Text, "+groupName="); ok {
					p.group = strings.TrimSpace(g)
				}
			}
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gd.Tok {
			case token.CONST:
				p.consts = append(p.consts, constsOf(gd, source)...)
			case token.TYPE:
				for _, spec := range gd.Specs {
					ts := spec.(*ast.TypeSpec)
					doc := ts.Doc
					if doc == nil && len(gd.Specs) == 1 {
						doc = gd.Doc
					}
					t := goType{name: ts.Name.Name, doc: docText(doc), source: source}
					t.st, _ = ts.Type.(*ast.StructType)
					p.types[t.name] = t
				}
			}
		}
	}
	if p.group == "" {
		return p, fmt.Errorf("%s: no +groupName marker", dir)
	}
	return p, nil
}

// constsOf returns the string constants of a const declaration. A constant's
// own comment wins; a lone constant takes the declaration's comment; in a
// block, the block's comment is the context.
func constsOf(gd *ast.GenDecl, source string) []goConst {
	var out []goConst
	blockDoc := ""
	if len(gd.Specs) > 1 {
		blockDoc = docText(gd.Doc)
	}
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		if len(vs.Names) != 1 || len(vs.Values) != 1 {
			continue
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		doc := vs.Doc
		if doc == nil && len(gd.Specs) == 1 {
			doc = gd.Doc
		}
		if doc == nil {
			doc = vs.Comment
		}
		typ := ""
		if id, ok := vs.Type.(*ast.Ident); ok {
			typ = id.Name
		}
		out = append(out, goConst{
			name: vs.Names[0].Name, typ: typ, value: value,
			doc: docText(doc), blockDoc: blockDoc, source: source,
		})
	}
	return out
}

func buildActions(pkgs []goPackage) (actionsDoc, error) {
	const union = "HostOperationAction"
	doc := actionsDoc{Actions: []action{}}
	for _, p := range pkgs {
		u, ok := p.types[union]
		if !ok || u.st == nil {
			continue
		}
		prefix, err := unionPath(p, union)
		if err != nil {
			return doc, err
		}
		doc.Kind = strings.TrimSuffix(prefix.owner, "Spec")
		doc.Source = u.source
		for _, f := range u.st.Fields.List {
			if len(f.Names) != 1 {
				continue
			}
			key := jsonName(f)
			typ := strings.TrimPrefix(types.ExprString(f.Type), "*")
			a := action{
				Key: key, GoName: f.Names[0].Name, Type: typ, Path: prefix.path + "[]." + key,
				Description: docText(f.Doc), Fields: []actionField{},
			}
			if t, ok := p.types[typ]; ok {
				a.TypeDescription = t.doc
				if t.st != nil {
					for _, af := range t.st.Fields.List {
						if len(af.Names) != 1 {
							continue
						}
						fd := actionField{
							Name: jsonName(af), GoName: af.Names[0].Name, GoType: types.ExprString(af.Type),
							Description: docText(af.Doc), Markers: nonNil(markers(af.Doc)),
						}
						a.NodeRef = a.NodeRef || fd.Name == "nodeRef"
						a.Fields = append(a.Fields, fd)
					}
				}
			}
			doc.Actions = append(doc.Actions, a)
		}
		return doc, nil
	}
	return doc, fmt.Errorf("no %s struct in the API packages", union)
}

// fillFromCRD takes each action field's type and whether it is required from
// the kind's CRD, and fails when an action or field has no row there: the Go
// union and the generated schema must describe the same thing.
func (d *actionsDoc) fillFromCRD(crds []crdDoc) error {
	var kind *crdDoc
	for i := range crds {
		if crds[i].Kind == d.Kind {
			kind = &crds[i]
		}
	}
	if kind == nil {
		return fmt.Errorf("actions: no CRD for kind %s", d.Kind)
	}
	rows := map[string]fieldRow{}
	for _, r := range kind.Fields {
		rows[r.Path] = r
	}
	for i := range d.Actions {
		a := &d.Actions[i]
		if _, ok := rows[a.Path]; !ok {
			return fmt.Errorf("actions: %s has no CRD field %s", a.Key, a.Path)
		}
		for j := range a.Fields {
			f := &a.Fields[j]
			r, ok := rows[a.Path+"."+f.Name]
			if !ok {
				return fmt.Errorf("actions: %s has no CRD field %s.%s", a.Key, a.Path, f.Name)
			}
			f.Type, f.Required = r.Type, r.Required
		}
	}
	return nil
}

type unionField struct{ owner, path string }

// unionPath finds the spec field that holds the action list
// ("spec.actions"), so each action links to its rows on the kind's page.
func unionPath(p goPackage, union string) (unionField, error) {
	for _, name := range sortedKeys(p.types) {
		t := p.types[name]
		if t.st == nil || !strings.HasSuffix(name, "Spec") {
			continue
		}
		for _, f := range t.st.Fields.List {
			if types.ExprString(f.Type) == "[]"+union && len(f.Names) == 1 {
				return unionField{owner: name, path: "spec." + jsonName(f)}, nil
			}
		}
	}
	return unionField{}, fmt.Errorf("no Spec struct has a []%s field", union)
}

func buildEnums(pkgs []goPackage) (enums enumsDoc, phases phasesDoc) {
	enums.Enums, phases.Phases = []enumDoc{}, []enumDoc{}
	for _, p := range pkgs {
		byType := map[string]*enumDoc{}
		var order []string
		for _, c := range p.consts {
			if c.typ == "" {
				continue
			}
			e, ok := byType[c.typ]
			if !ok {
				t := p.types[c.typ]
				e = &enumDoc{Type: c.typ, Group: p.group, Source: t.source, Description: t.doc, Kinds: []string{}, UsedBy: []string{}}
				byType[c.typ] = e
				order = append(order, c.typ)
			}
			e.Values = append(e.Values, enumValue{Name: c.name, Value: c.value, Description: c.doc})
		}
		for _, name := range sortedKeys(p.types) {
			t := p.types[name]
			if t.st == nil {
				continue
			}
			for _, f := range t.st.Fields.List {
				if len(f.Names) != 1 {
					continue
				}
				ft := strings.TrimLeft(types.ExprString(f.Type), "*[]")
				e, ok := byType[ft]
				if !ok {
					continue
				}
				e.UsedBy = append(e.UsedBy, name+"."+jsonName(f))
				if f.Names[0].Name == "Phase" && strings.HasSuffix(name, "Status") {
					e.Kinds = append(e.Kinds, strings.TrimSuffix(name, "Status"))
				}
			}
		}
		for _, typ := range order {
			e := byType[typ]
			enums.Enums = append(enums.Enums, *e)
			if len(e.Kinds) > 0 {
				phases.Phases = append(phases.Phases, *e)
			}
		}
	}
	return enums, phases
}

var condPair = regexp.MustCompile(`\b([A-Z][A-Za-z]+)=(True|False|Unknown)\b`)

func buildConditions(pkgs []goPackage) conditionsDoc {
	doc := conditionsDoc{Conditions: []constDoc{}, Reasons: []reasonDoc{}, OperationReasons: []constDoc{}}
	for _, p := range pkgs {
		for _, c := range p.consts {
			if c.typ != "" {
				continue
			}
			cd := constDoc{Name: c.name, Value: c.value, Group: p.group, Source: c.source, Description: stripName(c.name, c.doc)}
			switch {
			case strings.HasPrefix(c.name, "Condition"):
				doc.Conditions = append(doc.Conditions, cd)
			case strings.HasPrefix(c.name, "OperationReason"):
				doc.OperationReasons = append(doc.OperationReasons, cd)
			case strings.HasPrefix(c.name, "Reason"):
				r := reasonDoc{constDoc: cd, On: []condRef{}, Context: c.blockDoc}
				for _, m := range condPair.FindAllStringSubmatch(c.doc, -1) {
					ref := condRef{Condition: m[1], Status: m[2]}
					if !slices.Contains(r.On, ref) {
						r.On = append(r.On, ref)
					}
				}
				doc.Reasons = append(doc.Reasons, r)
			}
		}
	}
	return doc
}

// stripName drops the constant's own name from the front of its comment
// ("ReasonAborted on Executed=False: a person ..."), up to the colon, so
// the description starts with what it means.
func stripName(name, doc string) string {
	rest, ok := strings.CutPrefix(doc, name)
	if !ok {
		return doc
	}
	if after, ok := strings.CutPrefix(rest, ":"); ok {
		return strings.TrimSpace(after)
	}
	if strings.HasPrefix(rest, " on ") {
		if i := strings.Index(rest, ": "); i >= 0 && !strings.Contains(rest[:i], "\n\n") {
			return strings.TrimSpace(rest[i+1:])
		}
	}
	return doc
}

var keyValue = regexp.MustCompile(`^(metal|os)\.pk\.io/[a-z0-9][a-z0-9.-]*$`)

// scanKeys finds every string constant whose value is a metal.pk.io/ or
// os.pk.io/ key, in the API and in the code that uses keys of its own.
func scanKeys(cfg config) (keysDoc, error) {
	byKey := map[string]*keyDoc{}
	fset := token.NewFileSet()
	for _, dir := range cfg.scanDirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !isSourceFile(path) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, c := range constsOf(gd, relPath(cfg.root, path)) {
					if !keyValue.MatchString(c.value) {
						continue
					}
					k, ok := byKey[c.value]
					if !ok {
						k = &keyDoc{Key: c.value, Kind: keyKind(c.name), Constants: []keyConst{}}
						byKey[c.value] = k
					}
					exported := ast.IsExported(c.name)
					// The exported constant's comment is the one written for
					// readers of the API; prefer it over an internal one.
					if k.Description == "" || (exported && !k.Exported) {
						k.Description = c.doc
					}
					k.Exported = k.Exported || exported
					k.Constants = append(k.Constants, keyConst{Name: c.name, Source: c.source})
				}
			}
			return nil
		})
		if err != nil {
			return keysDoc{}, err
		}
	}
	doc := keysDoc{Keys: []keyDoc{}}
	for _, key := range sortedKeys(byKey) {
		doc.Keys = append(doc.Keys, *byKey[key])
	}
	return doc, nil
}

func keyKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "finalizer"), strings.Contains(n, "protection"):
		return "finalizer"
	case strings.Contains(n, "label"):
		return "label"
	case strings.Contains(n, "annotation"):
		return "annotation"
	}
	return "key"
}

func tagOf(f *ast.Field) string {
	if f.Tag == nil {
		return ""
	}
	tag, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return ""
	}
	return reflect.StructTag(tag).Get("json")
}

func jsonName(f *ast.Field) string {
	name, _, _ := strings.Cut(tagOf(f), ",")
	if name == "" && len(f.Names) == 1 {
		return f.Names[0].Name
	}
	return name
}

// markers returns the +marker lines of a comment without the "+" and without
// the "kubebuilder:validation:" prefix: "optional", "Minimum=0".
func markers(cg *ast.CommentGroup) []string {
	var out []string
	if cg == nil {
		return out
	}
	for line := range strings.SplitSeq(cg.Text(), "\n") {
		t := strings.TrimSpace(line)
		if m, ok := strings.CutPrefix(t, "+"); ok {
			out = append(out, strings.TrimPrefix(m, "kubebuilder:validation:"))
		}
	}
	return out
}

// docText is a Go comment as prose: markers removed, hard-wrapped lines joined
// into paragraphs, and indented code lines kept as they are.
func docText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	var paras []string
	for para := range strings.SplitSeq(cg.Text(), "\n\n") {
		var lines []string
		code := false
		for line := range strings.SplitSeq(para, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "+") {
				continue
			}
			if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "  ") {
				code = true
			}
			lines = append(lines, line)
		}
		var text string
		if code {
			text = strings.TrimRight(strings.Join(lines, "\n"), "\n")
		} else {
			text = joinWrapped(lines)
		}
		if strings.TrimSpace(text) != "" {
			paras = append(paras, text)
		}
	}
	return strings.Join(paras, "\n\n")
}
