package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// fixtureHelp answers `pk-metal <args>` from testdata/help: "help" reads
// help.txt, "rollout approve -h" reads rollout_approve.txt.
func fixtureHelp(args ...string) (string, error) {
	name := "help"
	if args[0] != "help" {
		name = strings.Join(args[:len(args)-1], "_")
	}
	b, err := os.ReadFile(filepath.Join("testdata", "help", name+".txt"))
	return string(b), err
}

// TestGolden runs docgen over the fixture API, CRD, RBAC and help text and
// compares every file it writes with testdata/golden.
func TestGolden(t *testing.T) {
	out := t.TempDir()
	cfg := config{
		root:          "testdata",
		out:           out,
		crdDir:        "testdata/crd",
		rbacDir:       "testdata/rbac",
		controllerDir: "testdata/controller",
		generatedRole: "pk-metal-manager",
		glossary:      "testdata/glossary.yaml",
		apiDirs:       []string{"testdata/api/v1alpha1"},
		scanDirs:      []string{"testdata/api"},
	}
	// A stale file from a removed kind must not survive a run.
	require.NoError(t, os.MkdirAll(filepath.Join(out, "crds"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(out, "crds", "gone.json"), []byte("{}"), 0o644))

	require.NoError(t, run(cfg, fixtureHelp))

	got := readTree(t, out)
	golden := filepath.Join("testdata", "golden")
	if *update {
		require.NoError(t, os.RemoveAll(golden))
		for rel, data := range got {
			path := filepath.Join(golden, rel)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, data, 0o644))
		}
	}
	want := readTree(t, golden)
	assert.NotContains(t, got, filepath.Join("crds", "gone.json"))
	require.ElementsMatch(t, keys(want), keys(got), "the set of files differs; run go test ./hack/docgen -update")
	for rel := range want {
		assert.Equal(t, string(want[rel]), string(got[rel]), "%s differs; run go test ./hack/docgen -update", rel)
	}
}

func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[rel], err = os.ReadFile(path)
		return err
	})
	require.NoError(t, err)
	return files
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseFlagsSection(t *testing.T) {
	text := "flags:\n" +
		"  -o string\n    \toutput format (default \"table\")\n" +
		"  -secrets talosctl gen secrets\n    \tthe bundle,\n    \tor - for stdin\n" +
		"  -context string\n    \tkubeconfig context (default: the current context)\n" +
		"  -wait\n    \twait (default true)\n"
	flags, err := parseFlagsSection("x\n" + text)
	require.NoError(t, err)
	assert.Equal(t, []cliFlag{
		{Name: "o", Type: "string", Default: "table", Usage: "output format"},
		{Name: "secrets", Type: "talosctl gen secrets", Usage: "the bundle, or - for stdin"},
		{Name: "context", Type: "string", Usage: "kubeconfig context (default: the current context)", Global: true},
		{Name: "wait", Type: "bool", Default: "true", Usage: "wait"},
	}, flags)

	_, err = parseFlagsSection("x\nflags:\nnot a flag line\n")
	require.Error(t, err, "an unknown line must fail, not vanish from the table")
}

func TestSubcommands(t *testing.T) {
	for _, tc := range []struct {
		synopsis string
		want     []string
	}{
		{"list | show <rollout> | approve [--yes] <rollout>", []string{"list", "show", "approve"}},
		{"serve [flags] | fleet --config <file>", []string{"serve", "fleet"}},
		{"[-n namespace] [--yes] [<operation> | <namespace>/<operation>]", nil},
		{"[--wait 2m] talos|kubernetes <cluster> <version>", nil},
		{"<bmc-url>", nil},
	} {
		var got []string
		for _, s := range subcommands(tc.synopsis, "x") {
			got = append(got, s.Name)
		}
		assert.Equal(t, tc.want, got, tc.synopsis)
	}
}

func TestStripName(t *testing.T) {
	assert.Equal(t, "the gate.", stripName("ConditionApproved", "ConditionApproved: the gate."))
	assert.Equal(t, "a person stopped it.", stripName("ReasonAborted", "ReasonAborted on Executed=False: a person stopped it."))
	assert.Equal(t, "OperationReasonBios is the reason.", stripName("OperationReasonBios", "OperationReasonBios is the reason."))
	assert.Equal(t, "Something else.", stripName("ReasonX", "Something else."))
}

func TestCheckGlossary(t *testing.T) {
	require.NoError(t, checkGlossary("testdata/glossary.yaml"))

	bad := filepath.Join(t.TempDir(), "glossary.yaml")
	require.NoError(t, os.WriteFile(bad, []byte(`domains:
  hw: { name: Hardware, icon: chip }
terms:
  - id: bmc
    term: BMC
    kind: abbr
    domain: hw
    forms: [BMC]
    see: concept:architecture
    related: [nope]
    def: A small computer.
  - id: Bad_ID
    term: Other
    kind: noun
    domain: nowhere
    forms: [BMC]
    see: https://example.org
    scope: site
    def: >-
      One two three four five six seven eight nine ten eleven twelve thirteen
      fourteen fifteen sixteen seventeen eighteen nineteen twenty twenty-one
      twenty-two twenty-three twenty-four twenty-five twenty-six.
`), 0o644))
	err := checkGlossary(bad)
	require.Error(t, err)
	for _, want := range []string{
		"needs its expansion", `related "nope"`, "lower case", `unknown domain "nowhere"`,
		`form "BMC" is also a form of "bmc"`, "is not a category link", "has 26 words", `scope "site" is not page`,
	} {
		assert.Contains(t, err.Error(), want)
	}

	unknown := filepath.Join(t.TempDir(), "glossary.yaml")
	require.NoError(t, os.WriteFile(unknown, []byte("domains: {}\nterms: []\ncolour: red\n"), 0o644))
	require.Error(t, checkGlossary(unknown), "an unknown key is a typo, not an extension")
}
