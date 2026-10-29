// Package work holds the kinds WorkItem and MilestoneList (work.pk.io). The
// status of a work item is derived from its linked changes, never stored.
package work

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group         = "work.pk.io"
	ItemName      = "WorkItem"
	MilestoneName = "MilestoneList"
)

// WorkItem is work.pk.io/v1alpha1 WorkItem (work/items/<id>.yaml). It is a
// flat document without metadata/spec, so people read it at a glance.
type WorkItem struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=work.pk.io/v1alpha1"`
	Kind       string   `json:"kind" yaml:"kind" jsonschema:"enum=WorkItem"`
	ID         string   `json:"id" yaml:"id" jsonschema:"title=Id,pattern=^w-[0-9]+$" pk:"truth=intent,label=Id,order=1"`
	Title      string   `json:"title" yaml:"title" jsonschema:"title=Title,minLength=3" pk:"truth=intent,label=Title,order=2"`
	Milestone  string   `json:"milestone,omitempty" yaml:"milestone,omitempty" jsonschema:"title=Milestone" pk:"truth=intent,ref=Milestone,widget=select,label=Milestone,order=3"`
	Owner      string   `json:"owner,omitempty" yaml:"owner,omitempty" jsonschema:"title=Owner" pk:"truth=intent,ref=Person,owner=people,widget=select,label=Owner,order=4"`
	Assets     []string `json:"assets,omitempty" yaml:"assets,omitempty" jsonschema:"title=Assets" pk:"truth=intent,ref=Entity,label=Assets,order=5"`
	Changes    []string `json:"changes,omitempty" yaml:"changes,omitempty" jsonschema:"title=Changes" pk:"truth=intent,ref=Change,label=Changes,order=6"`
	KB         []string `json:"kb,omitempty" yaml:"kb,omitempty" jsonschema:"title=Articles" pk:"truth=intent,ref=Article,label=Articles,order=7"`
	Notes      string   `json:"notes,omitempty" yaml:"notes,omitempty" pk:"truth=intent,label=Notes,order=8"`
}

// MilestoneList is work.pk.io/v1alpha1 MilestoneList (work/milestones.yaml).
type MilestoneList struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=work.pk.io/v1alpha1"`
	Kind       string      `json:"kind" yaml:"kind" jsonschema:"enum=MilestoneList"`
	Milestones []Milestone `json:"milestones" yaml:"milestones" pk:"truth=intent,label=Milestones"`
}

// Milestone groups work items.
type Milestone struct {
	ID          string `json:"id" yaml:"id" jsonschema:"pattern=^[a-z0-9-]+$" pk:"truth=intent,label=Id"`
	Title       string `json:"title" yaml:"title" pk:"truth=intent,label=Title"`
	Due         string `json:"due,omitempty" yaml:"due,omitempty" jsonschema:"format=date" pk:"truth=intent,label=Due"`
	Description string `json:"description,omitempty" yaml:"description,omitempty" pk:"truth=intent,label=Description"`
}

var (
	itemVersions      = []kindutil.Version{{Name: "v1alpha1", Type: WorkItem{}}}
	milestoneVersions = []kindutil.Version{{Name: "v1alpha1", Type: MilestoneList{}}}
)

// ItemKind returns the WorkItem kind.
func ItemKind() kinds.Kind {
	return kinds.Kind{
		Name:      ItemName,
		Group:     Group,
		Version:   "v1alpha1",
		Versions:  kindutil.Names(itemVersions...),
		Own:       true,
		Paths:     []string{"work/items/*.yaml"},
		NewPath:   func(name string) string { return "work/items/" + name + ".yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyNone,
		Views:     []string{"work"},
		Schema:    kindutil.SchemaFunc(Group, ItemName, "A piece of planned work, linked to changes.", itemVersions...),
		Validate:  validate,
		Describe:  describeItem,
		Skeleton:  ItemSkeleton,
	}
}

// MilestoneKind returns the MilestoneList kind.
func MilestoneKind() kinds.Kind {
	return kinds.Kind{
		Name:      MilestoneName,
		Group:     Group,
		Version:   "v1alpha1",
		Versions:  kindutil.Names(milestoneVersions...),
		Own:       true,
		Paths:     []string{"work/milestones.yaml"},
		NewPath:   func(string) string { return "work/milestones.yaml" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyNone,
		Schema:    kindutil.SchemaFunc(Group, MilestoneName, "Milestones of the work board.", milestoneVersions...),
		Describe:  kindutil.Describer(MilestoneName, "milestones"),
	}
}
