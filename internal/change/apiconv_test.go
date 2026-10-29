package change

import (
	"testing"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/policy"
)

func lifecycleOf(ch *Change) map[State]api.LifecycleStep {
	out := map[State]api.LifecycleStep{}
	for _, st := range LifecycleToAPI(ch) {
		out[State(st.State)] = st
	}
	return out
}

func TestLifecycleLabPassBeforePush(t *testing.T) {
	ch := &Change{}
	ch.State, ch.LabState, ch.LabRun = Draft, string(LabPass), "c-abcdef/run-1"
	l := lifecycleOf(ch)
	// The passing lab is the current step; the draft is done.
	if l[Draft].Status != api.LifecycleStepStatusDone || l[Draft].Label != "local" {
		t.Errorf("draft: %+v", l[Draft])
	}
	if l[Lab].Status != api.LifecycleStepStatusNow || l[Lab].Label != "run-1" {
		t.Errorf("lab: %+v", l[Lab])
	}
	if l[Review].Status != api.LifecycleStepStatusTodo || l[Review].Label != "no MR" {
		t.Errorf("review: %+v", l[Review])
	}
}

func TestLifecycleLabFail(t *testing.T) {
	ch := &Change{}
	ch.State, ch.LabState, ch.LabRun = Lab, string(LabFail), "c-abcdef/run-2"
	ch.MR = &MR{Number: 7, Draft: true}
	l := lifecycleOf(ch)
	if l[Lab].Status != api.LifecycleStepStatusFail {
		t.Errorf("lab: %+v", l[Lab])
	}
	if l[Draft].Label != "!7" || l[Review].Label != "draft MR" {
		t.Errorf("labels: %+v %+v", l[Draft], l[Review])
	}
}

func TestLifecycleMergedWithoutMR(t *testing.T) {
	ch := &Change{}
	ch.State, ch.MergeSHA = Merged, "0123456789abcdef"
	l := lifecycleOf(ch)
	if l[Review].Status != api.LifecycleStepStatusDone || l[Review].Label != "ready" {
		t.Errorf("review: %+v", l[Review])
	}
	if l[Merged].Status != api.LifecycleStepStatusNow || l[Merged].Label != "0123456" {
		t.Errorf("merged: %+v", l[Merged])
	}
}

func TestLifecycleLabStaleAfterRebase(t *testing.T) {
	ch := &Change{}
	// Ready and in review, but the run tested the head before the rebase.
	ch.State, ch.LabState, ch.LabRun = Review, "none", "c-abcdef/run-1"
	ch.MR = &MR{Number: 12}
	ch.Gates = &policy.Gates{}
	ch.Gates.LabGate.Required = true
	l := lifecycleOf(ch)
	if l[Lab].Status != api.LifecycleStepStatusTodo || l[Lab].Label != "run-1" || l[Lab].Detail != "run-1 tested an older head; run the lab again" {
		t.Errorf("lab: %+v", l[Lab])
	}
	if l[Draft].Status != api.LifecycleStepStatusDone || l[Review].Status != api.LifecycleStepStatusNow {
		t.Errorf("draft %+v review %+v", l[Draft], l[Review])
	}
	// Not required: the old run is only noted.
	ch.Gates.LabGate.Required = false
	l = lifecycleOf(ch)
	if l[Lab].Status != api.LifecycleStepStatusDone || l[Lab].Detail != "run-1 tested an older head" {
		t.Errorf("lab not required: %+v", l[Lab])
	}
	// Covered again after a new run.
	ch.Gates.LabGate.Required = true
	ch.LabState, ch.LabRun = string(LabPass), "c-abcdef/run-2"
	l = lifecycleOf(ch)
	if l[Lab].Status != api.LifecycleStepStatusDone || l[Lab].Detail != "" {
		t.Errorf("lab after new run: %+v", l[Lab])
	}
}
