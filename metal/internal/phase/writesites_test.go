package phase_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const module = "github.com/dennisklein/platform-kit/metal"

// guardedOwners are the status types whose Phase and BlockedOn summarise the
// conditions. OperationStep also has a Phase, but that one is the outcome of one
// action and the executor writes it directly.
var guardedOwners = map[string]bool{
	"HostStatus": true, "HostClaimStatus": true, "HostOperationStatus": true,
	"ManagementEndpointStatus": true,
}

// finding is one touch of a guarded field: where, and whether it writes.
type finding struct {
	Pos   string // file:line, relative to the module root
	Write bool
}

func (f finding) String() string {
	if f.Write {
		return f.Pos + " write"
	}
	return f.Pos + " read"
}

// TestPhaseWriteSites keeps "conditions are the truth" true as the code grows.
// Phase and BlockedOn are a summary that internal/phase derives from the
// conditions. Anything else that writes them publishes a summary that can
// disagree with its own conditions, and a reconciler that reads them decides
// on a summary instead of on the observations. The check is typed, so it also
// sees writes through an alias (st := &h.Status; st.Phase = x), through a
// pointer to the field, and in a composite literal. A text search sees none of
// these. Test files are out of scope: they build stale fixtures on purpose.
func TestPhaseWriteSites(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	fixture := module + "/internal/phase/testdata/writesites"
	cfg := &packages.Config{
		Dir:  root,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, "./...", fixture)
	require.NoError(t, err)
	for _, p := range pkgs {
		for _, e := range p.Errors {
			require.Failf(t, "load", "%s: %v", p.PkgPath, e)
		}
		// go list reads the sources in a child process, which go test's result
		// cache does not see. Stat them here so that editing any file of the
		// module, or adding one to a package, invalidates a cached pass.
		for _, f := range p.GoFiles {
			_, _ = os.Stat(f)
			_, _ = os.ReadDir(filepath.Dir(f))
		}
	}

	t.Run("the fixture's marked lines are exactly what the check finds", func(t *testing.T) {
		p := pkgs[slices.IndexFunc(pkgs, func(p *packages.Package) bool { return p.PkgPath == fixture })]
		got := check(root, p, false, true)
		want := wanted(t, root, p)
		assert.Equal(t, strs(want), strs(got))
	})

	t.Run("the module writes phase only in internal/phase and reconcilers never read it", func(t *testing.T) {
		var bad []finding
		for _, p := range pkgs {
			if p.PkgPath == fixture {
				continue
			}
			allowWrite := p.PkgPath == module+"/internal/phase"
			forbidRead := strings.HasPrefix(p.PkgPath, module+"/internal/controller")
			bad = append(bad, check(root, p, allowWrite, forbidRead)...)
		}
		assert.Empty(t, strs(bad), "write Phase and BlockedOn only through internal/phase (Stamp*), "+
			"and decide on conditions, not on the phase")
	})

	t.Run("the guard knows every summary status type", func(t *testing.T) {
		// A new top-level status with a Phase field must join guardedOwners, or
		// the check silently ignores it.
		api := pkgs[slices.IndexFunc(pkgs, func(p *packages.Package) bool { return p.PkgPath == module+"/api/v1alpha1" })]
		var owners []string
		for _, name := range api.Types.Scope().Names() {
			st, ok := api.Types.Scope().Lookup(name).Type().Underlying().(*types.Struct)
			if !ok || !strings.HasSuffix(name, "Status") {
				continue
			}
			for i := range st.NumFields() {
				if st.Field(i).Name() == "BlockedOn" {
					owners = append(owners, name)
				}
			}
		}
		assert.ElementsMatch(t, keys(guardedOwners), owners)
	})
}

// check reports every touch of a guarded field in p's non-test files that the
// policy forbids: any write unless allowWrite, and any read if forbidRead.
func check(root string, p *packages.Package, allowWrite, forbidRead bool) []finding {
	var out []finding
	report := func(n ast.Node, write bool) {
		if (write && allowWrite) || (!write && !forbidRead) {
			return
		}
		pos := p.Fset.Position(n.Pos())
		rel, _ := filepath.Rel(root, pos.Filename)
		out = append(out, finding{Pos: fmt.Sprintf("%s:%d", rel, pos.Line), Write: write})
	}
	for _, file := range p.Syntax {
		if strings.HasSuffix(p.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		// First pass: the selectors in a written position.
		written := map[*ast.SelectorExpr]bool{}
		mark := func(e ast.Expr) {
			if sel, ok := ast.Unparen(e).(*ast.SelectorExpr); ok {
				written[sel] = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					mark(l)
				}
			case *ast.IncDecStmt:
				mark(n.X)
			case *ast.RangeStmt:
				mark(n.Key)
				mark(n.Value)
			case *ast.UnaryExpr:
				// A pointer to the field is a write waiting to happen.
				if n.Op == token.AND {
					mark(n.X)
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if s := p.TypesInfo.Selections[n]; s != nil && s.Kind() == types.FieldVal && guarded(s.Recv(), n.Sel.Name) {
					report(n, written[n])
				}
			case *ast.CompositeLit:
				for _, el := range n.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && guarded(p.TypesInfo.TypeOf(n), key.Name) {
						report(kv, true)
					}
				}
			}
			return true
		})
	}
	return out
}

// guarded: the field named field of type t (or *t) is a summary Phase or
// BlockedOn of an API status type.
func guarded(t types.Type, field string) bool {
	if field != "Phase" && field != "BlockedOn" {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return strings.HasPrefix(named.Obj().Pkg().Path(), module+"/api/") && guardedOwners[named.Obj().Name()]
}

var wantRe = regexp.MustCompile(`// want (write|read)$`)

// wanted reads the "want" markers of the fixture package.
func wanted(t *testing.T, root string, p *packages.Package) []finding {
	var out []finding
	for _, name := range p.GoFiles {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		rel, _ := filepath.Rel(root, name)
		for i, line := range strings.Split(string(data), "\n") {
			if m := wantRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				out = append(out, finding{Pos: fmt.Sprintf("%s:%d", rel, i+1), Write: m[1] == "write"})
			}
		}
	}
	return out
}

func strs(fs []finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.String())
	}
	slices.Sort(out)
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
