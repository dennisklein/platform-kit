package v1alpha1

// Reasons added for day-2 convergence and scheduling. They live apart from
// conditions.go so that each concern's reasons are found next to each other.
const (
	// ConditionSystemFirmwareConverged: on a Host, whether the system
	// firmware (BIOS/UEFI, NIC, RAID) its FirmwareProfiles ask for is what
	// the inventory reports. Absent when no profile asks for any.
	ConditionSystemFirmwareConverged = "SystemFirmwareConverged"

	// ReasonNoMatchingComponent on SystemFirmwareConverged=False: a profile
	// names a component that no FirmwareInventory entry of the machine
	// matches. Nothing is proposed for a component that cannot be found.
	ReasonNoMatchingComponent = "NoMatchingComponent"

	// OperationReasonSystemFirmwareConvergence is the spec.reason of a
	// proposal that stages system firmware.
	OperationReasonSystemFirmwareConvergence = "SystemFirmwareConvergence"

	// ReasonInstalledNodeMissing on BiosConverged=False or
	// FirmwareConverged=False: the host's claim says it runs a TalosNode
	// that no longer exists. The node cannot be drained, so no change that
	// reboots the machine is proposed until the record and the node agree.
	ReasonInstalledNodeMissing = "InstalledNodeMissing"

	// ReasonOutsideMaintenanceWindow on Executed=Unknown of an approved
	// operation that has not started: a MaintenanceWindow covers its host
	// and none of the windows that cover it is open. The operation starts
	// when one opens; an operation already running is never stopped by a
	// window closing. Also the operation's blockedOn.
	ReasonOutsideMaintenanceWindow = "OutsideMaintenanceWindow"
)
