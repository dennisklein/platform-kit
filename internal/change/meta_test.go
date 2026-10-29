package change

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
)

func demoMeta() Meta {
	return Meta{
		ID: "c-k7m2qz", Title: "Add cn-035 and cn-036 to defiant",
		Intent: "2 compute nodes for tenants, in GC5A-6Z U6 and U8", Author: "dennis", Agent: "dennis-agent",
		Target: "main", WorkItems: []string{"w-114"},
		LabRuns: []LabRunRef{{Run: "c-k7m2qz/run-1", SHA: "abc", Result: LabPass, Report: "kb/labs/c-k7m2qz-run-1.md"}},
	}
}

func TestMetaRoundTrip(t *testing.T) {
	m := demoMeta()
	b := MarshalMeta(m)
	if !strings.HasPrefix(string(b), "apiVersion: pk/v1alpha1\nkind: Change\nid: c-k7m2qz\n") {
		t.Fatalf("meta file:\n%s", b)
	}
	got, err := ParseMeta(b)
	if err != nil {
		t.Fatal(err)
	}
	m.APIVersion, m.Kind = MetaAPIVersion, MetaKind
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip:\n%+v\n%+v", got, m)
	}
	if _, err := ParseMeta([]byte("title: x\n")); err == nil {
		t.Fatal("metadata without id must fail")
	}
}

func TestDescription(t *testing.T) {
	d := Description(demoMeta())
	want := `2 compute nodes for tenants, in GC5A-6Z U6 and U8

Pk-Change: c-k7m2qz
Pk-Title: Add cn-035 and cn-036 to defiant
Pk-Intent: 2 compute nodes for tenants, in GC5A-6Z U6 and U8
Pk-Lab-Run: c-k7m2qz/run-1 pass kb/labs/c-k7m2qz-run-1.md
Pk-Author: dennis
Pk-Agent: dennis-agent
Pk-Work-Item: w-114
`
	if d != want {
		t.Fatalf("description:\n%s\nwant:\n%s", d, want)
	}
	m, ok := MetaFromDescription(d)
	if !ok || m.ID != "c-k7m2qz" || m.Title != "Add cn-035 and cn-036 to defiant" || m.Agent != "dennis-agent" ||
		len(m.LabRuns) != 1 || m.LabRuns[0].Result != LabPass || m.LabRuns[0].Report != "kb/labs/c-k7m2qz-run-1.md" ||
		!reflect.DeepEqual(m.WorkItems, []string{"w-114"}) {
		t.Fatalf("parsed: %+v", m)
	}
	if _, ok := MetaFromDescription("just text"); ok {
		t.Fatal("no trailers must not parse")
	}
}

func TestMergeMessage(t *testing.T) {
	m := demoMeta()
	m.Agent = ""
	run := m.LabRuns[0]
	title, msg := MergeMessage(m, MergeInfo{MR: 12, ApprovedBy: []string{"anna", "jonas"}, LabRun: &run})
	if title != "Merge change c-k7m2qz: Add cn-035 and cn-036 to defiant (!12)" {
		t.Fatalf("title %q", title)
	}
	want := `2 compute nodes for tenants, in GC5A-6Z U6 and U8

Pk-Change: c-k7m2qz
Pk-Intent: 2 compute nodes for tenants, in GC5A-6Z U6 and U8
Pk-Lab-Run: c-k7m2qz/run-1 pass kb/labs/c-k7m2qz-run-1.md
Pk-Author: dennis
Pk-Work-Item: w-114
Approved-by: anna, jonas
Reviewed-in: !12`
	if msg != want {
		t.Fatalf("message:\n%s", msg)
	}
	ts := gitx.ParseTrailers(title + "\n\n" + msg)
	if Trailer(ts, TrailerChange) != "c-k7m2qz" || !reflect.DeepEqual(TrailerList(ts, TrailerApprovedBy), []string{"anna", "jonas"}) {
		t.Fatalf("trailers %+v", ts)
	}
	id, tt, mr := TitleFromMergeSubject(title)
	if id != "c-k7m2qz" || tt != m.Title || mr != 12 {
		t.Fatalf("subject parse: %s %s %d", id, tt, mr)
	}
}

func TestDeriveState(t *testing.T) {
	cases := []struct {
		name string
		in   StateInput
		want State
	}{
		{"draft", StateInput{}, Draft},
		{"draft MR", StateInput{HasMR: true, Draft: true}, Draft},
		{"lab running", StateInput{HasMR: true, Draft: true, Lab: "running"}, Lab},
		{"lab failed", StateInput{Lab: "fail"}, Lab},
		{"review", StateInput{HasMR: true, Lab: "pass", ApprovalsNeed: 2}, Review},
		{"review 1 of 2", StateInput{HasMR: true, ApprovalsHave: 1, ApprovalsNeed: 2}, Review},
		{"approved", StateInput{HasMR: true, ApprovalsHave: 2, ApprovalsNeed: 2}, Approved},
		{"merged", StateInput{Merged: true}, Merged},
		{"merged not deployed", StateInput{Merged: true, Rollout: []Rollout{{Cluster: "defiant"}}}, Merged},
		{"deployed", StateInput{Merged: true, Rollout: []Rollout{{Cluster: "defiant", Deployed: true}}}, Deployed},
		{"partly deployed", StateInput{Merged: true, Rollout: []Rollout{{Deployed: true, Verified: true}, {}}}, Merged},
		{"verified", StateInput{Merged: true, Rollout: []Rollout{{Deployed: true, Verified: true}}}, Verified},
		{"closed", StateInput{Closed: true, HasMR: true}, Closed},
	}
	for _, c := range cases {
		if got := DeriveState(c.in); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestLabCoversHead(t *testing.T) {
	if !LabCoversHead("c-k7m2qz", []string{".pk/changes/c-k7m2qz.yaml", "kb/labs/c-k7m2qz-run-1.md"}) {
		t.Fatal("metadata and report must not invalidate the run")
	}
	if !LabCoversHead("c-k7m2qz", nil) {
		t.Fatal("same sha must count")
	}
	if LabCoversHead("c-k7m2qz", []string{"control/cabling.yaml"}) {
		t.Fatal("a content change must invalidate the run")
	}
	if LabCoversHead("c-k7m2qz", []string{"kb/labs/c-p4x8ma-run-1.md"}) {
		t.Fatal("another change's report must invalidate the run")
	}
}

func TestGroupOf(t *testing.T) {
	cases := []struct {
		in   GroupInput
		want Group
	}{
		{GroupInput{Me: "dennis", Author: "dennis", AuthorKind: identity.Human}, GroupMine},
		{GroupInput{Me: "dennis", Author: "dennis-agent", AuthorKind: identity.Agent, AuthorActsFor: "dennis"}, GroupMine},
		{GroupInput{Me: "dennis", Author: "sync-bot", AuthorKind: identity.Bot}, GroupBots},
		{GroupInput{Me: "dennis", Author: "jonas", AuthorKind: identity.Human, Ready: true, CanApprove: true}, GroupReview},
		{GroupInput{Me: "dennis", Author: "jonas", AuthorKind: identity.Human, Ready: true, CanApprove: true, ApprovedByMe: true}, GroupOthers},
		{GroupInput{Me: "dennis", Author: "anna", AuthorKind: identity.Human}, GroupOthers},
	}
	for i, c := range cases {
		if got := GroupOf(c.in); got != c.want {
			t.Errorf("%d: %s, want %s", i, got, c.want)
		}
	}
}
