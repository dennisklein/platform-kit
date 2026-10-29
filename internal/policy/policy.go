// Package policy holds the policy model (.pk/policy.yaml) and the gate
// evaluation shared by the bench UI, the merge check and pk ci.
//
// The Go types here are the contract: the JSON Schema for the editor and the
// CI check is generated from them.
package policy

import (
	"context"
	"encoding/json"

	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// Path of the policy file in the state repo.
const Path = ".pk/policy.yaml"

// Policy is pk/v1alpha1 Policy, the root of all rights.
type Policy struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion" jsonschema:"enum=pk/v1alpha1"`
	Kind       string   `json:"kind" yaml:"kind" jsonschema:"enum=Policy"`
	People     []Person `json:"people" yaml:"people" jsonschema:"title=People,minItems=1" pk:"truth=intent,owner=people,label=People,order=1"`
	Agents     []Agent  `json:"agents,omitempty" yaml:"agents,omitempty" jsonschema:"title=Agents" pk:"truth=intent,label=Agents,order=2"`
	Bots       []Bot    `json:"bots,omitempty" yaml:"bots,omitempty" jsonschema:"title=Bots" pk:"truth=intent,label=Bots,order=3"`
	// Rules: first match wins, most specific first.
	Rules []Rule `json:"rules" yaml:"rules" jsonschema:"title=Path rules,minItems=1,description=First match wins; most specific first" pk:"truth=intent,label=Path rules,order=4"`
	// RiskRules rate the impact (rendered objects); the highest wins.
	RiskRules []RiskRule `json:"riskRules,omitempty" yaml:"riskRules,omitempty" jsonschema:"title=Risk rules" pk:"truth=intent,label=Risk rules,order=5"`
	// FastPath applies to revert changes.
	FastPath *FastPath `json:"fastPath,omitempty" yaml:"fastPath,omitempty" jsonschema:"title=Fast path for reverts" pk:"truth=intent,label=Fast path,order=6"`
}

// Person is a human with roles and teams.
type Person struct {
	ID    string   `json:"id" yaml:"id" jsonschema:"pattern=^[a-z][a-z0-9-]*$" pk:"label=Id"`
	Name  string   `json:"name" yaml:"name" pk:"label=Name"`
	Email string   `json:"email" yaml:"email" jsonschema:"format=email" pk:"label=Email"`
	Roles []string `json:"roles" yaml:"roles" jsonschema:"minItems=1" pk:"label=Roles"`
	Teams []string `json:"teams,omitempty" yaml:"teams,omitempty" pk:"label=Teams"`
}

// Right of a non-human identity.
type Right string

const (
	RightPush      Right = "push"
	RightOpenMR    Right = "open-mr"
	RightRunLab    Right = "run-lab"
	RightMarkReady Right = "mark-ready"
)

// Agent is the coding agent of one person. It never approves or merges.
type Agent struct {
	ID      string  `json:"id" yaml:"id" jsonschema:"pattern=^[a-z][a-z0-9-]*$" pk:"label=Id"`
	ActsFor string  `json:"actsFor" yaml:"actsFor" pk:"ref=Person,label=Acts for"`
	Can     []Right `json:"can" yaml:"can" jsonschema:"enum=push,enum=open-mr,enum=run-lab,enum=mark-ready" pk:"label=Rights"`
}

// Bot is a non-human author (sync bot).
type Bot struct {
	ID  string  `json:"id" yaml:"id" jsonschema:"pattern=^[a-z][a-z0-9-]*$" pk:"label=Id"`
	Can []Right `json:"can" yaml:"can" jsonschema:"enum=push,enum=open-mr,enum=run-lab,enum=mark-ready" pk:"label=Rights"`
}

// RiskLevel of a change.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Rank orders risk levels (low 1, medium 2, high 3; unknown 0).
func (r RiskLevel) Rank() int {
	switch r {
	case RiskLow:
		return 1
	case RiskMedium:
		return 2
	case RiskHigh:
		return 3
	}
	return 0
}

// Rule is a path rule.
//
// Writers and Approvers name principals: a role (owner, platform-admin,
// app-team), "team:<team>", "agent" (any agent whose person may write), or
// a bot id (sync-bot).
type Rule struct {
	Name      string    `json:"name" yaml:"name" pk:"label=Name"`
	Paths     []string  `json:"paths" yaml:"paths" jsonschema:"minItems=1,description=Doublestar globs" pk:"label=Paths"`
	Writers   []string  `json:"writers" yaml:"writers" pk:"label=Writers"`
	Approvers []string  `json:"approvers" yaml:"approvers" pk:"label=Approvers"`
	Approvals int       `json:"approvals" yaml:"approvals" jsonschema:"minimum=0,maximum=5" pk:"label=Approvals"`
	LabGate   bool      `json:"labGate,omitempty" yaml:"labGate,omitempty" pk:"label=Lab gate"`
	Risk      RiskLevel `json:"risk" yaml:"risk" jsonschema:"enum=low,enum=medium,enum=high" pk:"label=Risk"`
}

// RiskRule rates rendered objects.
type RiskRule struct {
	Match  ObjectMatch `json:"match" yaml:"match"`
	Risk   RiskLevel   `json:"risk" yaml:"risk" jsonschema:"enum=low,enum=medium,enum=high"`
	Reason string      `json:"reason" yaml:"reason"`
}

// ObjectMatch matches rendered objects; empty fields match all. Values are
// globs.
type ObjectMatch struct {
	Cluster    string `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name       string `json:"name,omitempty" yaml:"name,omitempty"`
}

// FastPath reduces gates for revert changes.
type FastPath struct {
	Approvals int  `json:"approvals" yaml:"approvals" jsonschema:"minimum=0"`
	LabGate   bool `json:"labGate" yaml:"labGate"`
}

// Schema returns the JSON Schema of the policy file.
func Schema() (json.RawMessage, error) {
	return schema.JSON(Policy{}, schema.Options{
		Title:       "Policy",
		Description: "platform-kit policy: people, agents, bots, path rules and risk rules.",
		APIVersion:  "pk/v1alpha1",
		Kind:        "Policy",
	})
}

// ---------------------------------------------------------------- gates

// LabState is the lab run state of a change head.
type LabState struct {
	// Status: none, running, pass, fail.
	Status string
	Run    string
	// SHA is the tested sha. The run counts for the head when the diff
	// between SHA and the head only touches .pk/changes/** and the run's
	// own report (kb/labs/<id>-run-<n>.md).
	SHA string
	// CoversHead is set by the caller after that check.
	CoversHead bool
}

// Review is an approval on the forge.
type Review struct {
	User string
	SHA  string
	// Stale: the approval is on an older head whose content differs from
	// the head (not only a rebase or records of the change). It never counts.
	Stale bool
}

// GateInput is everything the evaluation needs. Callers fill it from git,
// the forge, the lab store and the render.
type GateInput struct {
	ChangeID string
	// Author is the change author (person, agent or bot).
	Author identity.Identity
	// Writers are all identities that authored commits in the change.
	Writers []identity.Identity
	// Paths are the changed paths against the target.
	Paths []string
	// Objects are the rendered objects the change adds, changes or deletes.
	Objects []kinds.ObjectRef
	Reviews []Review
	Lab     LabState
	// Behind is the number of target commits the branch lacks.
	Behind int
	Target string
	// Revert enables the fast path.
	Revert bool
}

// PathRight is the write check of one path and one writer.
type PathRight struct {
	Path    string `json:"path"`
	Rule    string `json:"rule"`
	Who     string `json:"who"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// MatchedRule is a rule with the changed paths it matched.
type MatchedRule struct {
	Name      string    `json:"name"`
	Paths     []string  `json:"paths"`
	Approvals int       `json:"approvals"`
	LabGate   bool      `json:"labGate"`
	Risk      RiskLevel `json:"risk"`
	Approvers []string  `json:"approvers"`
}

// RiskReason explains a risk level.
type RiskReason struct {
	Reason string `json:"reason"`
	// Source: rule, riskRule or minimum.
	Source string `json:"source"`
	Rule   string `json:"rule,omitempty"`
	Object string `json:"object,omitempty"`
}

// Risk of a change.
type Risk struct {
	Level   RiskLevel    `json:"level"`
	Reasons []RiskReason `json:"reasons"`
}

// Approval is one review and whether it counts.
type Approval struct {
	User   string `json:"user"`
	SHA    string `json:"sha,omitempty"`
	Counts bool   `json:"counts"`
	Reason string `json:"reason,omitempty"`
}

// Gates is the result of the evaluation (mirrors the API schema Gates).
type Gates struct {
	PathRights struct {
		OK      bool        `json:"ok"`
		Writers []string    `json:"writers"`
		Details []PathRight `json:"details"`
	} `json:"pathRights"`
	LabGate struct {
		// Status: met, needed, running, failed, na.
		Status   string `json:"status"`
		Required bool   `json:"required"`
		Run      string `json:"run,omitempty"`
		SHA      string `json:"sha,omitempty"`
		Detail   string `json:"detail,omitempty"`
	} `json:"labGate"`
	Approvals struct {
		Have     int        `json:"have"`
		Need     int        `json:"need"`
		By       []string   `json:"by"`
		Eligible []string   `json:"eligible"`
		Ignored  []Approval `json:"ignored"`
	} `json:"approvals"`
	Rebased struct {
		OK     bool   `json:"ok"`
		Behind int    `json:"behind"`
		Target string `json:"target"`
	} `json:"rebased"`
	Risk         Risk          `json:"risk"`
	MatchedRules []MatchedRule `json:"matchedRules"`
	Mergeable    bool          `json:"mergeable"`
	Blockers     []string      `json:"blockers"`
	FastPath     bool          `json:"fastPath,omitempty"`
}

// BranchProtection is the generated forge rule for main.
type BranchProtection struct {
	Branch             string   `json:"branch"`
	RequiredChecks     []string `json:"requiredChecks"`
	MergeWhitelist     []string `json:"mergeWhitelist"`
	ApprovalsWhitelist []string `json:"approvalsWhitelist"`
	RequiredApprovals  int      `json:"requiredApprovals"`
	BlockOnOutdated    bool     `json:"blockOnOutdated"`
	EnablePush         bool     `json:"enablePush"`
}

// ForgeRules are the forge settings generated from the policy.
type ForgeRules struct {
	BranchProtection BranchProtection `json:"branchProtection"`
	// CodeOwners is the .gitea/CODEOWNERS content.
	CodeOwners string `json:"codeowners"`
}

// Engine evaluates the policy. One implementation serves the bench, the
// merge check and pk ci.
type Engine interface {
	// Load parses and validates a policy file.
	Load(data []byte) (*Policy, []kinds.Problem, error)
	// Current returns the policy on the target branch (main). Changes never
	// loosen their own gates: the target's policy applies.
	Current(ctx context.Context) (*Policy, error)
	// Evaluate computes the gates of a change.
	Evaluate(p *Policy, in GateInput) Gates
	// CanWrite reports the write rights of who for paths.
	CanWrite(p *Policy, who identity.Identity, paths []string) []PathRight
	// Identify returns the identity of a user id (person, agent or bot).
	Identify(p *Policy, user string) (identity.Identity, bool)
	// ForgeRules generates the branch protection and CODEOWNERS.
	ForgeRules(p *Policy) ForgeRules
}
