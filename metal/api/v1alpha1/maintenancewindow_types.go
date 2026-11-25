package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AnnotationIgnoreWindow on a HostOperation lets it start outside every
// maintenance window. It is for an urgent case that a person decides on: a
// security fix, or a machine that is down already. The value is
// "<username>: <reason>": who lifted the window and why. Admission accepts
// no other form, and only the caller's own username
// (config/policy/guard-lift.yaml). It shows in the record of the operation.
const AnnotationIgnoreWindow = "metal.pk.io/ignore-window"

// Weekday is a day of the week as a three-letter English abbreviation.
// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type Weekday string

// WeeklyWindow is a window that recurs every week on the given days.
type WeeklyWindow struct {
	// Days are the days the window opens on, in the window's time zone.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +listType=set
	Days []Weekday `json:"days"`

	// Start is the local time the window opens, as HH:MM (24-hour clock).
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`

	// Duration is how long the window stays open, such as 4h or 90m: at
	// least a minute, at most a week. A window may run past midnight.
	Duration metav1.Duration `json:"duration"`
}

// OnceWindow is a single window between two points in time.
//
// +kubebuilder:validation:XValidation:rule="self.end > self.start",message="end must be after start"
type OnceWindow struct {
	// Start is when the window opens (RFC 3339, with its offset).
	Start metav1.Time `json:"start"`
	// End is when it closes.
	End metav1.Time `json:"end"`
}

// MaintenanceWindowSpec says which machines the window covers and when it is
// open.
//
// +kubebuilder:validation:XValidation:rule="has(self.hostSelector) || (has(self.clusters) && size(self.clusters) > 0)",message="name the machines: hostSelector, clusters, or both"
// +kubebuilder:validation:XValidation:rule="(has(self.weekly) && size(self.weekly) > 0) || (has(self.once) && size(self.once) > 0)",message="give at least one weekly or once window"
type MaintenanceWindowSpec struct {
	// HostSelector selects Hosts by label, the same labels profiles match
	// (metal.pk.io/vendor, metal.pk.io/model, and the claim's own labels).
	// An empty selector ({}) selects every Host.
	// +optional
	HostSelector *metav1.LabelSelector `json:"hostSelector,omitempty"`

	// Clusters names TalosClusters (os.pk.io): a Host whose node belongs to
	// one of them is covered. A Host is covered when it matches the
	// selector or runs a node of a named cluster.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Clusters []string `json:"clusters,omitempty"`

	// TimeZone is the IANA time zone the weekly windows are read in, such as
	// Europe/Berlin; daylight saving time is followed. Default UTC.
	// +kubebuilder:default=UTC
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// Weekly are windows that recur every week.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Weekly []WeeklyWindow `json:"weekly,omitempty"`

	// Once are single windows, for a planned intervention outside the
	// usual schedule.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Once []OnceWindow `json:"once,omitempty"`
}

// MaintenanceWindowStatus is what the operator reads from the spec: whether
// the window is open now and when that changes.
type MaintenanceWindowStatus struct {
	// Open is true while one of the windows is open.
	// +optional
	Open bool `json:"open,omitempty"`

	// NextChange is when Open flips next: the close of the window that is
	// open, or the start of the next one. Empty when no window comes again.
	// +optional
	NextChange *metav1.Time `json:"nextChange,omitempty"`

	// Conditions: Valid is False when the time zone or the selector cannot
	// be read. An invalid window still covers the hosts that it can name, and
	// it is never open. Thus a mistake in a window stops work and does not let
	// it run.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MaintenanceWindow says when disruptive work may start on a set of
// machines. An approved HostOperation on a covered machine waits, with
// blockedOn OutsideMaintenanceWindow, until one of the windows that cover
// it opens; then it starts. A machine covered by no window is not held.
// Work already running is never stopped by a window closing: stopping a
// machine half-way through a change leaves it worse off than finishing.
//
// New installs (operations with reason Provisioning) are not held: they
// disturb no running workload. To let one operation start now anyway:
//
//	kubectl annotate hostoperation -n pk-metal <name> metal.pk.io/ignore-window="<why>"
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Open",type=boolean,JSONPath=`.status.open`
// +kubebuilder:printcolumn:name="Next change",type=date,JSONPath=`.status.nextChange`
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.timeZone`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MaintenanceWindow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaintenanceWindowSpec `json:"spec"`
	// +optional
	Status MaintenanceWindowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MaintenanceWindowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceWindow `json:"items"`
}

// ConditionValid: on a MaintenanceWindow, whether the operator can read it.
const ConditionValid = "Valid"

// Reasons on the window's Valid condition.
const (
	// ReasonWindowValid on Valid=True.
	ReasonWindowValid = "WindowValid"
	// ReasonWindowInvalid on Valid=False: the message says what is wrong.
	ReasonWindowInvalid = "WindowInvalid"
)

// The kind registers itself, so that adding it needs no edit of the shared
// kind list in groupversion_info.go.
func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &MaintenanceWindow{}, &MaintenanceWindowList{})
		return nil
	})
}
