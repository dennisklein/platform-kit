package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/llm"
)

func seedState(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "seed", "state", p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func platformFake(t *testing.T) *fake {
	f := newFake(t)
	for _, p := range []string{
		"control/talos/defiant/talconfig.yaml", "control/talos/phoenix/talconfig.yaml",
		"clusters/defiant/cluster.yaml", "clusters/pegasus/cluster.yaml", "clusters/phoenix/cluster.yaml",
		"clusters/defiant/infra/cilium/helmrelease.yaml",
	} {
		f.files["main\x00"+p] = seedState(t, p)
		f.files[cid+"\x00"+p] = seedState(t, p)
	}
	for _, n := range []string{"cn-025", "cn-026", "cp-01", "cp-02", "cp-03"} {
		p := "control/inventory/" + n + ".yaml"
		f.files[cid+"\x00"+p] = seedState(t, p)
	}
	return f
}

func TestPlatformVersions(t *testing.T) {
	s, _, _ := newSvc(t, platformFake(t), llm.NewMock(nil))
	for _, c := range []struct{ q, want string }{
		{"Which Kubernetes version runs on defiant?", "**defiant** runs Kubernetes **v1.33.4** on Talos **v1.10.6**. That is the desired state on main, in `control/talos/defiant/talconfig.yaml`"},
		{"Which Talos version runs on defiant?", "**defiant** runs Talos **v1.10.6**, with Kubernetes **v1.33.4**."},
		{"What k8s version is phoenix on?", "**phoenix** runs Kubernetes **v1.33.4**"},
		{"What Kubernetes version do the clusters run?", "Both clusters with a talconfig, defiant and phoenix, run Kubernetes **v1.33.4** on Talos **v1.10.6**."},
		{"Which Talos version runs on pegasus?", "**pegasus** has no talconfig in the state repo"},
		{"Is defiant on the latest Kubernetes?", "I can't check upstream releases"},
	} {
		a, err := s.Ask(ctxAs("dennis"), ask(c.q))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(a.Text, c.want) || strings.Contains(a.Text, "cilium") {
			t.Errorf("%q: %q", c.q, a.Text)
		}
		if len(a.Sources) == 0 || a.Sources[0].Label != "talconfig.yaml" {
			t.Errorf("%q: sources %+v", c.q, a.Sources)
		}
		noFailedStep(t, a)
	}
	// The edit names the field of the talconfig.
	a, _ := s.Ask(ctxAs("dennis"), ask("Upgrade Kubernetes on defiant to v1.34.1"))
	if !strings.Contains(a.Text, "`control/talos/defiant/talconfig.yaml` (`kubernetesVersion`, now v1.33.4)") {
		t.Errorf("edit: %q", a.Text)
	}
}

func TestCountNodesByRole(t *testing.T) {
	s, _, _ := newSvc(t, platformFake(t), llm.NewMock(nil))
	a, _ := s.Ask(ctxAs("dennis"), ask("How many control plane nodes does phoenix have?"))
	if a.Text != "**phoenix** has **3 control plane nodes** (cp-01, cp-02 and cp-03)." {
		t.Errorf("%q", a.Text)
	}
	a, _ = s.Ask(ctxAs("dennis"), ask("How many worker nodes are there?"))
	if !strings.HasPrefix(a.Text, "The inventory has **2 worker nodes** (cn-025 and cn-026).") {
		t.Errorf("%q", a.Text)
	}
}

func TestFileAnswerNamesTheQuestion(t *testing.T) {
	const hr = "clusters/defiant/infra/cilium/helmrelease.yaml"
	h := llm.Hit{Document: llm.Document{Kind: "file", Ref: hr, Path: hr, Text: seedState(t, hr)}}
	q := "Which Kubernetes version runs on defiant?"
	if got := fileAnswer(h, q, contentTerms(q)); got != "The file `"+hr+"` on main matches best. Open it from the source below." {
		t.Errorf("kubernetes: %q", got)
	}
	q = "Which chart does cilium use on defiant?"
	if got := fileAnswer(h, q, contentTerms(q)); !strings.HasPrefix(got, "**cilium** is a HelmRelease on defiant: chart cilium **1.18.2**") {
		t.Errorf("cilium: %q", got)
	}
	const tc = "control/talos/defiant/talconfig.yaml"
	got, ok := fileSummary(tc, seedState(t, tc), nil)
	if !ok || got != "`talconfig.yaml` pins Talos **v1.10.6** and Kubernetes **v1.33.4** for the cluster **defiant** (`talosVersion`, `kubernetesVersion`)." {
		t.Errorf("talconfig: %v %q", ok, got)
	}
}
