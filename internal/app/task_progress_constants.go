package app

const (
	taskTypeInstallApp      = "install_app"
	taskTypeStartApp        = "start_app"
	taskTypeStopApp         = "stop_app"
	taskTypeUninstallApp    = "uninstall_app"
	taskTypeUpdateListeners = "update_listeners"
	taskTypeUpdateImage     = "update_image"
	taskTypeRollbackApp     = "rollback_app"
)

const (
	taskPhaseAllocatingPorts     = "allocating_ports"
	taskPhaseCleaningVolumes     = "cleaning_volumes"
	taskPhaseComplete            = "complete"
	taskPhaseCreatingContainer   = "creating_container"
	taskPhaseFinalizing          = "finalizing"
	taskPhasePullingImage        = "pulling_image"
	taskPhaseReconcilingServices = "reconciling_services"
	taskPhaseRecreatingContainer = "recreating_container"
	taskPhaseRegisteringServices = "registering_services"
	taskPhaseRemovingContainer   = "removing_container"
	taskPhaseStarting            = "starting"
	taskPhaseStopping            = "stopping"
	taskPhaseUpdatingServices    = "updating_services"
	taskPhaseValidating          = "validating"

	// Update-specific phases
	taskPhaseSnapshotting   = "snapshotting"
	taskPhaseCreatingRootfs = "creating_rootfs"

	// Init script phases
	taskPhaseRunningInit = "running_init"

	// Workspace disk phases
	taskPhaseMountingWorkspace = "mounting_workspace"
	taskPhaseUnmountingWorkspace = "unmounting_workspace"
	taskPhaseInitializingDisk    = "initializing_disk"
)
