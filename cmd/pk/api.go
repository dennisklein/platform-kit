package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	pkapi "github.com/dennisklein/platform-kit/api"
	"github.com/dennisklein/platform-kit/internal/config"
)

// addAPICommands adds "pk <tag> <operation>" commands generated at run time
// from the embedded OpenAPI spec (api.Spec), plus the friendly aliases
// (pk change new, pk change list, pk edit, pk lab run, pk agent approve).
// Base URL PK_URL (default http://127.0.0.1:8080), token PK_TOKEN.
func addAPICommands(root *cobra.Command) {
	ops, tags, err := specOps(pkapi.Spec)
	if err != nil {
		// The spec is embedded; a broken one is a build bug.
		fmt.Fprintln(os.Stderr, "pk: api commands:", err)
		return
	}
	var asJSON bool
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON (API commands)")
	out := func() bool { return asJSON }

	tagCmds := map[string]*cobra.Command{}
	for _, t := range tags {
		c := &cobra.Command{Use: t.name, Short: t.desc, Args: cobra.ArbitraryArgs,
			RunE: func(c *cobra.Command, args []string) error {
				if len(args) > 0 {
					return fmt.Errorf("unknown command %q for %q (see pk %s --help)", args[0], c.CommandPath(), c.Name())
				}
				return c.Help()
			}}
		if s := singular(t.name); s != t.name {
			c.Aliases = []string{s}
		}
		if existing := findCmd(root, t.name); existing != nil {
			continue
		}
		tagCmds[t.name] = c
		root.AddCommand(c)
	}
	for _, op := range ops {
		parent := tagCmds[op.tag]
		if parent == nil {
			continue
		}
		parent.AddCommand(opCommand(op, out))
	}
	if c := tagCmds["changes"]; c != nil {
		c.AddCommand(changeNewCmd(out), changeListCmd(out))
	}
	if c := tagCmds["labs"]; c != nil {
		c.AddCommand(labRunCmd(out))
	}
	if c := tagCmds["agents"]; c != nil {
		c.AddCommand(agentApproveCmd(out))
	}
	root.AddCommand(editCmd(out))
}

func findCmd(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// ---------------------------------------------------------------- spec

type specTag struct{ name, desc string }

type specParam struct {
	name, in, desc, typ string
	required            bool
	enum                []string
	array               bool
}

type bodyProp struct {
	name, desc, typ string
	array           bool
	required        bool
}

type specOp struct {
	id, tag, method, path, summary, mcp string
	params                              []specParam
	body                                bool
	bodyRequired                        bool
	props                               []bodyProp
	produces                            string
}

func specOps(spec []byte) ([]specOp, []specTag, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, nil, err
	}
	var tags []specTag
	if l, ok := doc["tags"].([]any); ok {
		for _, x := range l {
			m, _ := x.(map[string]any)
			n, _ := m["name"].(string)
			d, _ := m["description"].(string)
			if n != "" {
				tags = append(tags, specTag{n, d})
			}
		}
	}
	resolve := func(m map[string]any) map[string]any {
		for i := 0; i < 5 && m != nil; i++ {
			ref, ok := m["$ref"].(string)
			if !ok {
				return m
			}
			var cur any = doc
			for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
				cm, _ := cur.(map[string]any)
				cur = cm[part]
			}
			m, _ = cur.(map[string]any)
		}
		return m
	}
	paths, _ := doc["paths"].(map[string]any)
	var ops []specOp
	for path, raw := range paths {
		item, _ := raw.(map[string]any)
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			o, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			op := specOp{method: strings.ToUpper(method), path: path}
			op.id, _ = o["operationId"].(string)
			op.summary, _ = o["summary"].(string)
			op.mcp, _ = o["x-mcp-tool"].(string)
			if tl, ok := o["tags"].([]any); ok && len(tl) > 0 {
				op.tag, _ = tl[0].(string)
			}
			var params []any
			if l, ok := item["parameters"].([]any); ok {
				params = append(params, l...)
			}
			if l, ok := o["parameters"].([]any); ok {
				params = append(params, l...)
			}
			for _, x := range params {
				p := resolve(asMap(x))
				sp := specParam{}
				sp.name, _ = p["name"].(string)
				sp.in, _ = p["in"].(string)
				sp.desc, _ = p["description"].(string)
				sp.required, _ = p["required"].(bool)
				sch := resolve(asMap(p["schema"]))
				sp.typ, _ = sch["type"].(string)
				if sp.typ == "array" {
					sp.array = true
					sp.typ, _ = resolve(asMap(sch["items"]))["type"].(string)
				}
				if e, ok := sch["enum"].([]any); ok {
					for _, v := range e {
						sp.enum = append(sp.enum, fmt.Sprint(v))
					}
				}
				if sp.name != "" && (sp.in == "path" || sp.in == "query") {
					op.params = append(op.params, sp)
				}
			}
			if rb := resolve(asMap(o["requestBody"])); rb != nil {
				op.body = true
				op.bodyRequired, _ = rb["required"].(bool)
				ct := asMap(rb["content"])
				sch := resolve(asMap(asMap(ct["application/json"])["schema"]))
				req := map[string]bool{}
				if l, ok := sch["required"].([]any); ok {
					for _, r := range l {
						req[fmt.Sprint(r)] = true
					}
				}
				for name, pv := range asMap(sch["properties"]) {
					ps := resolve(asMap(pv))
					bp := bodyProp{name: name, required: req[name]}
					bp.desc, _ = ps["description"].(string)
					bp.typ, _ = ps["type"].(string)
					if bp.typ == "array" {
						bp.array = true
						bp.typ, _ = resolve(asMap(ps["items"]))["type"].(string)
					}
					if bp.typ == "" && ps["enum"] != nil {
						bp.typ = "string"
					}
					op.props = append(op.props, bp)
				}
				sort.Slice(op.props, func(i, j int) bool { return op.props[i].name < op.props[j].name })
			}
			for code, r := range asMap(o["responses"]) {
				if !strings.HasPrefix(code, "2") {
					continue
				}
				for ct := range asMap(resolve(asMap(r))["content"]) {
					op.produces = ct
				}
			}
			if op.id != "" && op.tag != "" {
				ops = append(ops, op)
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].id < ops[j].id })
	return ops, tags, nil
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// kebab turns listChanges into list-changes.
func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pathParams returns the path parameters in path order.
func (op specOp) pathParams() []specParam {
	var out []specParam
	rest := op.path
	for {
		i := strings.Index(rest, "{")
		if i < 0 {
			return out
		}
		j := strings.Index(rest[i:], "}")
		name := rest[i+1 : i+j]
		for _, p := range op.params {
			if p.in == "path" && p.name == name {
				out = append(out, p)
			}
		}
		rest = rest[i+j:]
	}
}

// ---------------------------------------------------------------- generated commands

func opCommand(op specOp, asJSON func() bool) *cobra.Command {
	pp := op.pathParams()
	use := kebab(op.id)
	for _, p := range pp {
		use += " <" + p.name + ">"
	}
	short := op.summary
	if short == "" {
		short = op.method + " " + op.path
	}
	cmd := &cobra.Command{Use: use, Short: short, Long: fmt.Sprintf("%s\n\n%s %s%s", short, op.method, "/api/v1", op.path)}
	if op.mcp != "" {
		cmd.Long += "\nMCP tool: " + op.mcp
	}
	strs := map[string]*string{}
	slices := map[string]*[]string{}
	flagNames := map[string]bool{"json": true, "data": true, "data-file": true, "verbose": true}
	for _, p := range op.params {
		desc := p.desc
		if len(p.enum) > 0 {
			desc = strings.TrimSpace(desc + " (" + strings.Join(p.enum, ", ") + ")")
		}
		if p.in == "path" {
			desc = strings.TrimSpace(desc + " (or the positional argument)")
		}
		flagNames[p.name] = true
		if p.array {
			slices[p.name] = cmd.Flags().StringSlice(p.name, nil, desc)
		} else {
			strs[p.name] = cmd.Flags().String(p.name, "", desc)
		}
	}
	var data, dataFile string
	bodyStrs := map[string]*string{}
	bodySlices := map[string]*[]string{}
	if op.body {
		cmd.Flags().StringVar(&data, "data", "", "request body as JSON (or @file, - for stdin)")
		cmd.Flags().StringVar(&dataFile, "data-file", "", "file with the request body (JSON or YAML)")
		for _, bp := range op.props {
			if flagNames[bp.name] || (bp.typ != "string" && bp.typ != "integer" && bp.typ != "boolean" && bp.typ != "number") {
				continue
			}
			flagNames[bp.name] = true
			name := kebab(bp.name)
			if bp.array {
				bodySlices[bp.name] = cmd.Flags().StringSlice(name, nil, bp.desc)
			} else {
				bodyStrs[bp.name] = cmd.Flags().String(name, "", strings.TrimSpace(bp.desc+" (body field)"))
			}
		}
	}
	cmd.Args = cobra.MaximumNArgs(len(pp))
	cmd.RunE = func(c *cobra.Command, args []string) error {
		path := op.path
		for i, p := range pp {
			v := *strs[p.name]
			if i < len(args) {
				v = args[i]
			}
			if v == "" {
				return fmt.Errorf("%s is required", p.name)
			}
			path = strings.ReplaceAll(path, "{"+p.name+"}", url.PathEscape(v))
		}
		q := url.Values{}
		for _, p := range op.params {
			if p.in != "query" {
				continue
			}
			if s, ok := slices[p.name]; ok {
				for _, v := range *s {
					q.Add(p.name, v)
				}
			} else if v := *strs[p.name]; v != "" {
				q.Set(p.name, v)
			} else if p.required {
				return fmt.Errorf("--%s is required", p.name)
			}
		}
		var body any
		if op.body {
			b, err := readBody(data, dataFile)
			if err != nil {
				return err
			}
			body = b
			fields := map[string]any{}
			for _, bp := range op.props {
				if s, ok := bodyStrs[bp.name]; ok && c.Flags().Changed(kebab(bp.name)) {
					fields[bp.name] = typed(*s, bp.typ)
				}
				if s, ok := bodySlices[bp.name]; ok && c.Flags().Changed(kebab(bp.name)) {
					l := []any{}
					for _, v := range *s {
						l = append(l, typed(v, bp.typ))
					}
					fields[bp.name] = l
				}
			}
			if len(fields) > 0 {
				m, ok := body.(map[string]any)
				if body != nil && !ok {
					return errors.New("--data must be a JSON object to combine with field flags")
				}
				if m == nil {
					m = map[string]any{}
				}
				for k, v := range fields {
					m[k] = v
				}
				body = m
			}
			if body == nil && op.bodyRequired {
				body = map[string]any{}
			}
		}
		cl := newAPIClient()
		if op.produces == "text/event-stream" || op.produces == "application/x-ndjson" {
			return cl.stream(c.Context(), op.method, path, q, c.OutOrStdout())
		}
		raw, err := cl.do(c.Context(), op.method, path, q, body)
		if err != nil {
			return err
		}
		return printResult(c.OutOrStdout(), raw, asJSON())
	}
	return cmd
}

// typed parses a flag value as the schema type.
func typed(s, typ string) any {
	switch typ {
	case "integer", "number", "boolean":
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v
		}
	}
	return s
}

func readBody(data, file string) (any, error) {
	var b []byte
	var err error
	switch {
	case file != "":
		b, err = os.ReadFile(file)
	case data == "-":
		b, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(data, "@"):
		b, err = os.ReadFile(data[1:])
	case data != "":
		b = []byte(data)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil { // YAML is a superset of JSON
		return nil, fmt.Errorf("request body: %w", err)
	}
	return v, nil
}

// printResult prints JSON (--json) or YAML in the key order of the answer.
func printResult(w io.Writer, raw []byte, asJSON bool) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if asJSON {
		var buf bytes.Buffer
		if json.Indent(&buf, raw, "", "  ") != nil {
			_, err := w.Write(append(raw, '\n'))
			return err
		}
		buf.WriteByte('\n')
		_, err := buf.WriteTo(w)
		return err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(raw, &n); err != nil {
		_, err := w.Write(append(raw, '\n'))
		return err
	}
	blockStyle(&n)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return err
	}
	return enc.Close()
}

func blockStyle(n *yaml.Node) {
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style = 0
	}
	if n.Kind == yaml.ScalarNode && n.Style == yaml.DoubleQuotedStyle {
		n.Style = 0
		if strings.Contains(n.Value, "\n") {
			n.Style = yaml.LiteralStyle
		}
	}
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// ---------------------------------------------------------------- client

type apiClient struct {
	base, token string
	hc          *http.Client
}

func newAPIClient() *apiClient {
	base := strings.TrimRight(os.Getenv("PK_URL"), "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	token := os.Getenv("PK_TOKEN")
	if token == "" {
		token = os.Getenv("PK_ADMIN_TOKEN")
	}
	if token == "" {
		cfg := config.FromEnv()
		for _, p := range []string{filepath.Join(cfg.StateDir(), "admin-token"), filepath.Join(cfg.DataDir, "admin-token")} {
			if b, err := os.ReadFile(p); err == nil {
				token = strings.TrimSpace(string(b))
				break
			}
		}
	}
	return &apiClient{base: base, token: token, hc: &http.Client{Timeout: 5 * time.Minute}}
}

// APIError is an error answer of the bench.
type APIError struct {
	Status int
	Msg    string
	Detail string
	Paths  []string
}

func (e *APIError) Error() string {
	s := e.Msg
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}
	if len(e.Paths) > 0 {
		s += ": " + strings.Join(e.Paths, ", ")
	}
	return fmt.Sprintf("%s [%d]", s, e.Status)
}

func (c *apiClient) request(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	u := c.base + "/api/v1" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "pk/"+version)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bench at %s: %w (start it with pk bench, or set PK_URL)", c.base, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		e := &APIError{Status: resp.StatusCode}
		var v struct {
			Error  string   `json:"error"`
			Detail string   `json:"detail"`
			Paths  []string `json:"paths"`
		}
		if json.Unmarshal(b, &v) == nil && v.Error != "" {
			e.Msg, e.Detail, e.Paths = v.Error, v.Detail, v.Paths
		} else {
			e.Msg = strings.TrimSpace(string(b))
			if e.Msg == "" {
				e.Msg = resp.Status
			}
		}
		if resp.StatusCode == http.StatusUnauthorized && c.token == "" {
			e.Detail = "set PK_TOKEN, or PK_DATA to the bench data directory"
		}
		return nil, e
	}
	return resp, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	resp, err := c.request(ctx, method, path, q, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *apiClient) json(ctx context.Context, method, path string, q url.Values, body, out any) error {
	b, err := c.do(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *apiClient) stream(ctx context.Context, method, path string, q url.Values, w io.Writer) error {
	hc := *c.hc
	hc.Timeout = 0
	cc := *c
	cc.hc = &hc
	resp, err := cc.request(ctx, method, path, q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// ---------------------------------------------------------------- aliases

type changeSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Branch    string `json:"branch"`
	State     string `json:"state"`
	LabState  string `json:"labState"`
	Author    string `json:"author"`
	Agent     string `json:"agent"`
	Risk      string `json:"risk"`
	Files     int    `json:"files"`
	Dirty     bool   `json:"dirty"`
	Approvals struct {
		Have int `json:"have"`
		Need int `json:"need"`
	} `json:"approvals"`
}

func changeNewCmd(asJSON func() bool) *cobra.Command {
	var intent, target, parent string
	var work []string
	cmd := &cobra.Command{
		Use:   "new <title>",
		Short: "New change (worktree, branch and metadata file)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body := map[string]any{"title": strings.Join(args, " ")}
			if intent != "" {
				body["intent"] = intent
			}
			if target != "" {
				body["target"] = target
			}
			if parent != "" {
				body["parent"] = parent
			}
			if len(work) > 0 {
				body["workItems"] = work
			}
			raw, err := newAPIClient().do(c.Context(), http.MethodPost, "/changes", nil, body)
			if err != nil {
				return err
			}
			if asJSON() {
				return printResult(c.OutOrStdout(), raw, true)
			}
			var ch changeSummary
			if err := json.Unmarshal(raw, &ch); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s  %s\nbranch %s\nnext: pk edit <entity> --set path=value --change %s\n", ch.ID, ch.Title, ch.Branch, ch.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "why (goes into the merge request)")
	cmd.Flags().StringVar(&target, "target", "", "main (default) or the branch of an open change")
	cmd.Flags().StringVar(&parent, "parent", "", "parent change id (stack)")
	cmd.Flags().StringSliceVar(&work, "work-item", nil, "work item ids (repeat)")
	return cmd
}

func changeListCmd(asJSON func() bool) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Open changes as a table",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			q := url.Values{}
			if all {
				q.Set("scope", "all")
			}
			raw, err := newAPIClient().do(c.Context(), http.MethodGet, "/changes", q, nil)
			if err != nil {
				return err
			}
			if asJSON() {
				return printResult(c.OutOrStdout(), raw, true)
			}
			var list struct {
				Changes []changeSummary `json:"changes"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATE\tLAB\tRISK\tAPPROVALS\tAUTHOR\tTITLE")
			for _, ch := range list.Changes {
				author := ch.Author
				if ch.Agent != "" {
					author += " (" + ch.Agent + ")"
				}
				title := ch.Title
				if ch.Dirty {
					title += " *"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\n", ch.ID, ch.State, dash(ch.LabState), dash(ch.Risk),
					ch.Approvals.Have, ch.Approvals.Need, author, title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "add recent merges and closed changes")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// currentChange is --change, PK_CHANGE or the change in focus.
func currentChange(ctx context.Context, cl *apiClient, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("PK_CHANGE"); v != "" {
		return v, nil
	}
	var f struct {
		ChangeID string `json:"changeId"`
	}
	if err := cl.json(ctx, http.MethodGet, "/focus", nil, nil, &f); err == nil && f.ChangeID != "" {
		return f.ChangeID, nil
	}
	return "", errors.New("no change: pass --change, set PK_CHANGE, or open a change in the web UI")
}

type entitySummary struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func editCmd(asJSON func() bool) *cobra.Command {
	var sets []string
	var changeID string
	cmd := &cobra.Command{
		Use:   "edit <entity>",
		Short: "Set fields of an entity in a change (pk edit cn-035 --set spec.location.unit=8)",
		Long: "Set fields of an entity by dotted path in a change worktree. The entity is kind/name or a\n" +
			"unique name. Values are YAML (8, true, data-b, [a, b], {switch: mgmt, port: 37}). The change is --change, PK_CHANGE\n" +
			"or the change in focus. The file is validated; problems are printed.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(sets) == 0 {
				return errors.New("nothing to set: use --set path=value")
			}
			ctx := c.Context()
			cl := newAPIClient()
			id, err := currentChange(ctx, cl, changeID)
			if err != nil {
				return err
			}
			fields := map[string]any{}
			for _, s := range sets {
				k, v, ok := strings.Cut(s, "=")
				if !ok || k == "" {
					return fmt.Errorf("--set %q: want path=value", s)
				}
				var val any = v
				var y any
				if v != "" && yaml.Unmarshal([]byte(v), &y) == nil {
					switch y.(type) {
					case int, float64, bool, nil, []any, map[string]any:
						val = y
					case string:
						// "2.4" keeps a number-like value a string.
						if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'") {
							val = y
						}
					}
				}
				fields[k] = val
			}
			ent, err := findEntity(ctx, cl, id, args[0])
			if err != nil {
				return err
			}
			raw, err := cl.do(ctx, http.MethodPut, "/changes/"+url.PathEscape(id)+"/file", url.Values{"path": {ent.Path}}, map[string]any{"set": fields})
			if err != nil {
				return err
			}
			if asJSON() {
				return printResult(c.OutOrStdout(), raw, true)
			}
			var res struct {
				File struct {
					Problems []struct {
						Severity string `json:"severity"`
						Message  string `json:"message"`
						Line     int    `json:"line"`
					} `json:"problems"`
				} `json:"file"`
				Impact struct {
					Text string `json:"text"`
				} `json:"impact"`
			}
			_ = json.Unmarshal(raw, &res)
			fmt.Fprintf(c.OutOrStdout(), "saved %s in %s (%s/%s)\n", ent.Path, id, ent.Kind, ent.Name)
			for _, p := range res.File.Problems {
				fmt.Fprintf(c.OutOrStdout(), "  %s line %d: %s\n", p.Severity, p.Line, p.Message)
			}
			if res.Impact.Text != "" {
				fmt.Fprintln(c.OutOrStdout(), "impact:", res.Impact.Text)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "path=value (repeat)")
	cmd.Flags().StringVar(&changeID, "change", "", "change id (default PK_CHANGE or the change in focus)")
	return cmd
}

func findEntity(ctx context.Context, cl *apiClient, changeID, arg string) (entitySummary, error) {
	q := url.Values{"ref": {changeID}}
	kind, name, hasKind := strings.Cut(arg, "/")
	if hasKind {
		q.Set("kind", kind)
	} else {
		name = arg
	}
	var list []entitySummary
	if err := cl.json(ctx, http.MethodGet, "/entities", q, nil, &list); err != nil {
		return entitySummary{}, err
	}
	var hits []entitySummary
	for _, e := range list {
		if e.Name == name && (!hasKind || strings.EqualFold(e.Kind, kind)) {
			hits = append(hits, e)
		}
	}
	switch len(hits) {
	case 0:
		return entitySummary{}, fmt.Errorf("no entity %s in %s", arg, changeID)
	case 1:
		return hits[0], nil
	}
	var kinds []string
	for _, h := range hits {
		kinds = append(kinds, h.Kind+"/"+h.Name)
	}
	return entitySummary{}, fmt.Errorf("%s is ambiguous: %s", arg, strings.Join(kinds, ", "))
}

type labRun struct {
	ID       string `json:"id"`
	Result   string `json:"result"`
	Summary  string `json:"summary"`
	Scenario string `json:"scenario"`
	Steps    []struct {
		Name   string `json:"name"`
		Title  string `json:"title"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"steps"`
	Failures []string `json:"failures"`
}

func labRunCmd(asJSON func() bool) *cobra.Command {
	var scenario, changeID string
	var wait bool
	cmd := &cobra.Command{
		Use:   "run [change]",
		Short: "Start a change lab (default: the change in focus); --wait follows it to the end",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			cl := newAPIClient()
			if len(args) == 1 {
				changeID = args[0]
			}
			id, err := currentChange(ctx, cl, changeID)
			if err != nil {
				return err
			}
			var body any
			if scenario != "" {
				body = map[string]any{"scenario": scenario}
			}
			var run labRun
			raw, err := cl.do(ctx, http.MethodPost, "/changes/"+url.PathEscape(id)+"/labs", nil, body)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &run); err != nil {
				return err
			}
			out := c.OutOrStdout()
			if !wait {
				if asJSON() {
					return printResult(out, raw, true)
				}
				fmt.Fprintf(out, "%s started (%s)\nfollow: pk lab run --wait, or pk labs get-lab-run %s\n", run.ID, dash(run.Scenario), run.ID)
				return nil
			}
			if !asJSON() {
				fmt.Fprintf(out, "%s started (%s)\n", run.ID, dash(run.Scenario))
			}
			shown := 0
			for {
				for ; shown < len(run.Steps) && run.Steps[shown].Status != "" && run.Steps[shown].Status != "running" && run.Steps[shown].Status != "pending"; shown++ {
					s := run.Steps[shown]
					if !asJSON() {
						title := s.Title
						if title == "" {
							title = s.Name
						}
						fmt.Fprintf(out, "  %-7s %s", s.Status, title)
						if s.Detail != "" {
							fmt.Fprintf(out, ": %s", s.Detail)
						}
						fmt.Fprintln(out)
					}
				}
				if run.Result != "running" {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				raw, err = cl.do(ctx, http.MethodGet, "/labs/runs/"+url.PathEscape(run.ID), nil, nil)
				if err != nil {
					return err
				}
				run = labRun{}
				if err := json.Unmarshal(raw, &run); err != nil {
					return err
				}
			}
			if asJSON() {
				return printResult(out, raw, true)
			}
			fmt.Fprintf(out, "%s: %s %s\n", run.ID, run.Result, run.Summary)
			for _, f := range run.Failures {
				fmt.Fprintln(out, "  -", f)
			}
			if run.Result == "fail" {
				return fmt.Errorf("lab %s failed", run.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scenario, "scenario", "", "scenario (default from the changed paths)")
	cmd.Flags().StringVar(&changeID, "change", "", "change id (default PK_CHANGE or the change in focus)")
	cmd.Flags().BoolVar(&wait, "wait", false, "follow the run until it ends")
	return cmd
}

func agentApproveCmd(asJSON func() bool) *cobra.Command {
	var mode string
	var hours int
	cmd := &cobra.Command{
		Use:   "approve [agent]",
		Short: "Approve your agent for today and print its MCP setup (default <you>-agent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			cl := newAPIClient()
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else {
				var me struct {
					ID string `json:"id"`
				}
				if err := cl.json(ctx, http.MethodGet, "/me", nil, nil, &me); err != nil {
					return err
				}
				id = me.ID + "-agent"
			}
			body := map[string]any{}
			if mode != "" {
				body["mode"] = mode
			}
			if hours != 0 {
				body["hours"] = hours
			}
			raw, err := cl.do(ctx, http.MethodPost, "/agents/"+url.PathEscape(id)+"/approve", nil, body)
			if err != nil {
				return err
			}
			if asJSON() {
				return printResult(c.OutOrStdout(), raw, true)
			}
			var a struct {
				Token  string `json:"token"`
				McpURL string `json:"mcpUrl"`
				Grant  struct {
					Mode      string    `json:"mode"`
					ExpiresAt time.Time `json:"expiresAt"`
				} `json:"grant"`
				Snippets []struct {
					Client, File, Content, Command, Allow string
					AllowFlag                             string `json:"allowFlag"`
				} `json:"snippets"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "%s approved, %s mode, until %s\ntoken (shown once): %s\nMCP: %s\n", id, a.Grant.Mode,
				a.Grant.ExpiresAt.Local().Format("15:04"), a.Token, a.McpURL)
			for _, s := range a.Snippets {
				fmt.Fprintf(out, "\n# %s (%s)\n", s.Client, s.File)
				if s.Command != "" {
					fmt.Fprintln(out, s.Command)
				} else {
					fmt.Fprintln(out, s.Content)
				}
				if s.Allow != "" {
					fmt.Fprintln(out, "# Allow the platform-kit tools in this directory (else the agent asks at each new tool):")
					fmt.Fprintln(out, s.Allow)
					if s.AllowFlag != "" {
						fmt.Fprintf(out, "# Other way: start the agent with %s\n", s.AllowFlag)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "navigate (default: your view follows the agent) or suggest")
	cmd.Flags().IntVar(&hours, "hours", 0, "hours, 1 to 12 (default 8)")
	return cmd
}
