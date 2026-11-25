// Package audit writes the operator's audit trail: one JSON line for each
// decision about a machine. A decision is a proposal, an approval, a lifted
// guard, a step that starts, ends or fails, an operation or rollout that
// ends, and a record that the operator deletes.
//
// Every line has the same fixed fields (ts, logger, seq, pod, event, kind,
// namespace, name, uid, actor), and each event adds only the fields that its
// method takes. The call sites cannot add a field or misspell one: the methods are
// the whole interface. A line holds names, reasons and plan summaries. It
// never holds a config, a token or a password: no method takes one, and
// the free text a method takes (a step's error) is masked with
// internal/redact before it is written, since a BMC's error can echo a grant
// URL with its credentials.
//
// The lines go to the writer the Logger was made with (the manager's
// standard error, next to its other log lines). Each one carries
// "logger":"audit", so a site can route the trail by that field alone, for
// example to Loki. The lines are not written through the manager's logr
// logger, because its encoder writes its own ts field, and the schema here
// must not depend on how the manager encodes its other lines.
//
// seq counts the lines of one process from 1, and pod names the process's
// pod. A reader who has every line of a pod sees a gap or a reordering as a
// missing or repeated number; a restart starts a new pod name or a new count
// at 1. The numbers do not make the trail tamper-proof: a site ships the
// lines to storage that the cluster cannot rewrite.
//
// A nil *Logger writes nothing, so a reconciler that a test builds without
// one runs as before.
package audit

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/redact"
)

// LoggerName is the value of the logger field of every line.
const LoggerName = "audit"

// Kind is the kind of the object that a line is about.
type Kind string

// The kinds that a line can be about. A guard-lift line can name the object
// that carries the annotation, which need not be an operation.
const (
	KindHostOperation Kind = "HostOperation"
	KindTalosRollout  Kind = "TalosRollout"
	KindHostClaim     Kind = "HostClaim"
	KindTalosNode     Kind = "TalosNode"
	KindHost          Kind = "Host"
	// KindManagementEndpoint: a BMC, for what the operator does to it
	// outside an operation (EndpointProfile lanes, the TLS key pin).
	KindManagementEndpoint Kind = "ManagementEndpoint"
)

// Event is what happened.
type Event string

// The events.
const (
	EventProposed     Event = "proposed"
	EventApproved     Event = "approved"
	EventGuardLifted  Event = "guard.lifted"
	EventStepStarted  Event = "step.started"
	EventStepFinished Event = "step.finished"
	EventStepFailed   Event = "step.failed"
	EventFinished     Event = "finished"
	EventDeleted      Event = "deleted"
	// EventMediaFetched: the first request a media grant admitted. The
	// grant is bound to that source address.
	EventMediaFetched Event = "media.fetched"
	// EventMediaRefused: a request named a media grant and was refused.
	EventMediaRefused Event = "media.refused"
	// EventMediaGrantEnded: a media grant was revoked or expired; the line
	// says what it served.
	EventMediaGrantEnded Event = "media.grantEnded"

	// The operator's own changes to a BMC, which no operation gates.
	EventBMCAccountSet      Event = "bmc.account.set"
	EventBMCAccountDisabled Event = "bmc.account.disabled"
	EventBMCIdentityPushed  Event = "bmc.identity.pushed"
	EventBMCRestarted       Event = "bmc.restarted"
	EventBMCKeyPinned       Event = "bmc.tls.pinned"
)

// DeleteReason says why the operator deletes a record.
type DeleteReason string

// The reasons for a delete.
const (
	// DeleteRetentionExpired: the record finished longer ago than the site
	// keeps finished records.
	DeleteRetentionExpired DeleteReason = "RetentionExpired"
	// DeleteWithdrawn: a proposal that nobody approved no longer fits the
	// situation (a superseded plan, a node that is gone, a held claim).
	DeleteWithdrawn DeleteReason = "Withdrawn"
	// DeleteStopped: an unfinished operation whose purpose is gone (the
	// intent was dropped, the claim or the node is being deleted). A
	// running one stops between steps.
	DeleteStopped DeleteReason = "Stopped"
	// DeleteRetry: a person asked for a failed proposal again (pk-metal
	// retry); the operator replaces it with a new proposal.
	DeleteRetry DeleteReason = "RetryRequested"
	// DeleteRecordOfRemoved: the record belongs to a claim or a node that
	// is gone, and other records keep what it decided.
	DeleteRecordOfRemoved DeleteReason = "RecordOfRemovedObject"
	// DeleteOwnerDeleted: the record goes with its owner (an operation of
	// a rollout that the operator deletes).
	DeleteOwnerDeleted DeleteReason = "OwnerDeleted"
)

// Object identifies the object that a line is about.
type Object struct {
	Kind      Kind
	Namespace string
	Name      string
	UID       types.UID
}

// Line is one line of the trail. The fields up to Actor are in every line;
// the others are in the lines of the events that the comments name. A
// pointer field is a pointer so that false and zero are written where the
// event has them, and nothing where it does not.
type Line struct {
	TS     string `json:"ts"`
	Logger string `json:"logger"`
	// Seq is the number of the line in this process, from 1.
	Seq uint64 `json:"seq"`
	// Pod is the pod of the process that wrote the line; empty outside a
	// pod.
	Pod       string `json:"pod,omitempty"`
	Event     Event  `json:"event"`
	Kind      Kind   `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	// Actor is who made the decision: the proposer, the approver, the
	// person a guard lift names, or the operator.
	Actor string `json:"actor"`

	// proposed, approved
	ProposedBy string `json:"proposedBy,omitempty"`
	// proposed
	Target           string `json:"target,omitempty"`
	Plan             string `json:"plan,omitempty"`
	ApprovalRequired *bool  `json:"approvalRequired,omitempty"`
	// approved
	ApprovedBy   string `json:"approvedBy,omitempty"`
	SelfApproved *bool  `json:"selfApproved,omitempty"`
	PlanHash     string `json:"planHash,omitempty"`
	// guard.lifted
	Annotation string `json:"annotation,omitempty"`
	Value      string `json:"value,omitempty"`
	Named      *bool  `json:"named,omitempty"`
	Operation  string `json:"operation,omitempty"`
	// step.started, step.finished, step.failed; guard.lifted at a step
	Step   *int   `json:"step,omitempty"`
	Action string `json:"action,omitempty"`
	// step.finished, step.failed
	Result string `json:"result,omitempty"`
	// step.failed, finished, deleted
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// finished
	Phase           string   `json:"phase,omitempty"`
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`
	// media.fetched, media.refused, media.grantEnded: the grant's file
	// (never its token) and the source address. reason is set on
	// media.refused and media.grantEnded.
	Grant  string `json:"grant,omitempty"`
	Source string `json:"source,omitempty"`
	// media.grantEnded
	Requests *int64 `json:"requests,omitempty"`
	Bytes    *int64 `json:"bytes,omitempty"`
	Refused  *int   `json:"refused,omitempty"`
	// bmc.account.*
	Account string `json:"account,omitempty"`
	// bmc.identity.pushed, bmc.tls.pinned: a certificate fingerprint or a
	// key, never key material.
	Key string `json:"key,omitempty"`
}

// maxMessage bounds the free text of a line. The full text stays on the
// object; the trail needs enough to say what happened.
const maxMessage = 512

// Logger writes the trail.
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
	// operator is the operator's username: the actor of what the operator
	// decides on its own (steps, the end of a run, a delete).
	operator string
	pod      string
	seq      uint64
	now      func() time.Time
}

// New returns a Logger that writes to w. operator is the operator's own
// username.
func New(w io.Writer, operator string) *Logger {
	enc := json.NewEncoder(w)
	// A reason can hold < or >; the trail is read as text, not embedded in
	// HTML.
	enc.SetEscapeHTML(false)
	return &Logger{enc: enc, operator: operator, now: time.Now}
}

// WithClock replaces the clock; for tests.
func (l *Logger) WithClock(now func() time.Time) *Logger {
	l.now = now
	return l
}

// WithPod sets the pod name that every line carries.
func (l *Logger) WithPod(pod string) *Logger {
	l.pod = pod
	return l
}

// Operator is the username the Logger writes as the operator's.
func (l *Logger) Operator() string {
	if l == nil {
		return ""
	}
	return l.operator
}

// Proposed records that an operation or a rollout was proposed: by the
// operator, or by a person or GitOps, seen by the operator for the first
// time. target is the machine or the cluster; plan says what it does.
func (l *Logger) Proposed(o Object, proposedBy, target, plan string, approvalRequired bool) {
	l.write(o, EventProposed, proposedBy, func(x *Line) {
		x.ProposedBy, x.Target, x.Plan, x.ApprovalRequired = proposedBy, target, plan, &approvalRequired
	})
}

// Approved records an approval. planHash is the plan that a rollout's
// approval names; empty for an operation.
func (l *Logger) Approved(o Object, proposedBy, approvedBy, planHash string) {
	self := approvedBy == proposedBy
	l.write(o, EventApproved, approvedBy, func(x *Line) {
		x.ProposedBy, x.ApprovedBy, x.SelfApproved, x.PlanHash = proposedBy, approvedBy, &self, planHash
	})
}

// GuardLifted records that a guard-lift annotation on o was in effect for
// a decision of the operator: the start of an operation, the proposal of
// one, or the release of a node. operation names the operation the
// decision was about when that is another object than o. The actor is the
// person the value names ("<username>: <reason>"); named is false, and the
// actor empty, when the value names nobody (a value set before admission
// asked for a name, or an annotation that takes any value).
func (l *Logger) GuardLifted(o Object, annotation, value, operation string) {
	l.guardLifted(o, annotation, value, func(x *Line) { x.Operation = operation })
}

// GuardLiftedAtStep records that a guard-lift annotation on the operation o
// was in effect when the operator decided on step (counted from 0) again.
func (l *Logger) GuardLiftedAtStep(o Object, annotation, value string, step int, action string) {
	l.guardLifted(o, annotation, value, func(x *Line) { x.Step, x.Action = &step, action })
}

func (l *Logger) guardLifted(o Object, annotation, value string, fill func(*Line)) {
	actor, _, named := metalv1alpha1.ParseGuardLift(value)
	l.write(o, EventGuardLifted, actor, func(x *Line) {
		x.Annotation, x.Value, x.Named = annotation, clip(value), &named
		fill(x)
	})
}

// StepStarted records that a run moved on to step (counted from 0).
func (l *Logger) StepStarted(o Object, step int, action string) {
	l.write(o, EventStepStarted, l.Operator(), func(x *Line) { x.Step, x.Action = &step, action })
}

// StepFinished records that step reached its goal; result is how it ended
// (Succeeded for an operation; the node state for a rollout).
func (l *Logger) StepFinished(o Object, step int, action, result string) {
	l.write(o, EventStepFinished, l.Operator(), func(x *Line) { x.Step, x.Action, x.Result = &step, action, result })
}

// StepFailed records that step failed, with the reason and the message the
// object's status carries.
func (l *Logger) StepFailed(o Object, step int, action, reason, message string) {
	l.write(o, EventStepFailed, l.Operator(), func(x *Line) {
		x.Step, x.Action, x.Result, x.Reason, x.Message = &step, action, "Failed", reason, clip(redact.String(message))
	})
}

// Finished records the end of an operation or a rollout: its phase, the
// reason of its outcome, and how long it ran (zero when it never started).
func (l *Logger) Finished(o Object, phase, reason string, ran time.Duration) {
	secs := ran.Round(time.Second).Seconds()
	l.write(o, EventFinished, l.Operator(), func(x *Line) { x.Phase, x.Reason, x.DurationSeconds = phase, reason, &secs })
}

// Deleted records that the operator is about to delete o. It is written
// before the delete request, so that the record of the delete exists even
// when the request is the last thing the process does.
func (l *Logger) Deleted(o Object, reason DeleteReason, message string) {
	l.write(o, EventDeleted, l.Operator(), func(x *Line) { x.Reason, x.Message = string(reason), clip(redact.String(message)) })
}

// MediaFetched records the first request a media grant of the operation o
// admitted. grant is the file the grant serves; source is the address the
// grant is bound to from now on.
func (l *Logger) MediaFetched(o Object, grant, source string) {
	l.write(o, EventMediaFetched, l.Operator(), func(x *Line) { x.Grant, x.Source = grant, source })
}

// MediaRefused records a refused request for a media grant of o.
func (l *Logger) MediaRefused(o Object, grant, source, reason string) {
	l.write(o, EventMediaRefused, l.Operator(), func(x *Line) {
		x.Grant, x.Source, x.Reason = grant, source, clip(reason)
	})
}

// MediaGrantEnded records the end of a media grant of o: how it ended
// (revoked, expired), the address it was bound to (empty when nobody
// fetched it), and what it served and refused.
func (l *Logger) MediaGrantEnded(o Object, grant, source, how string, requests, bytes int64, refused int) {
	l.write(o, EventMediaGrantEnded, l.Operator(), func(x *Line) {
		x.Grant, x.Source, x.Reason = grant, source, how
		x.Requests, x.Bytes, x.Refused = &requests, &bytes, &refused
	})
}

// BMCAccountSet records that the operator set the password of its managed
// account on a BMC; reason says why (Adopted, Rotated, Renamed, Repaired).
// The password itself is never written.
func (l *Logger) BMCAccountSet(o Object, account, reason string) {
	l.write(o, EventBMCAccountSet, l.Operator(), func(x *Line) { x.Account, x.Reason = account, reason })
}

// BMCAccountDisabled records that the operator disabled an account on a BMC.
func (l *Logger) BMCAccountDisabled(o Object, account, reason string) {
	l.write(o, EventBMCAccountDisabled, l.Operator(), func(x *Line) { x.Account, x.Reason = account, reason })
}

// BMCIdentityPushed records that the operator gave a BMC a new TLS
// certificate; key is the certificate's fingerprint.
func (l *Logger) BMCIdentityPushed(o Object, key, message string) {
	l.write(o, EventBMCIdentityPushed, l.Operator(), func(x *Line) { x.Key, x.Message = key, clip(message) })
}

// BMCRestarted records that the operator restarted a BMC (action is the
// reset type), and why.
func (l *Logger) BMCRestarted(o Object, action, reason string) {
	l.write(o, EventBMCRestarted, l.Operator(), func(x *Line) { x.Action, x.Reason = action, reason })
}

// BMCKeyPinned records the TLS key the operator trusts for a BMC from now
// on; reason is how it came to be trusted (FirstUse, Approved, Pushed).
func (l *Logger) BMCKeyPinned(o Object, key, reason string) {
	l.write(o, EventBMCKeyPinned, l.Operator(), func(x *Line) { x.Key, x.Reason = key, reason })
}

func (l *Logger) write(o Object, ev Event, actor string, fill func(*Line)) {
	if l == nil {
		return
	}
	x := Line{
		Logger: LoggerName, Event: ev, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name,
		UID: string(o.UID), Actor: actor,
	}
	fill(&x)
	l.mu.Lock()
	defer l.mu.Unlock()
	x.TS = l.now().UTC().Format(time.RFC3339Nano)
	l.seq++
	x.Seq, x.Pod = l.seq, l.pod
	// A write that fails has nowhere better to go: the writer is the log.
	_ = l.enc.Encode(&x)
}

func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	n := maxMessage
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
