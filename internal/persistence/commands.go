package persistence

import (
	"context"

	"piccolod/internal/runtime/commands"
)

const (
	CommandEnsureVolume    = "persistence.ensure_volume"
	CommandAttachVolume    = "persistence.attach_volume"
	CommandRecordLockState = "persistence.record_lock_state"
)

// EnsureVolumeCommand requests creation (or retrieval) of a volume matching
// the provided request parameters.
type EnsureVolumeCommand struct {
	Req VolumeRequest
}

func (c EnsureVolumeCommand) Name() string { return CommandEnsureVolume }

type EnsureVolumeResponse struct {
	Handle VolumeHandle
}

// AttachVolumeCommand requests the module to attach a volume using the
// specified options (e.g., leader/follower mode).
type AttachVolumeCommand struct {
	Handle VolumeHandle
	Opts   AttachOptions
}

func (c AttachVolumeCommand) Name() string { return CommandAttachVolume }

// RecordLockStateCommand notifies the persistence module about the current
// control-store lock state so it can broadcast to other components.
type RecordLockStateCommand struct {
	Locked bool
}

func (c RecordLockStateCommand) Name() string { return CommandRecordLockState }

func (m *Module) handleEnsureVolume(ctx context.Context, cmd commands.Command) (commands.Response, error) {
	request, ok := cmd.(EnsureVolumeCommand)
	if !ok {
		return nil, ErrInvalidCommand
	}
	handle, err := m.volumes.EnsureVolume(ctx, request.Req)
	if err != nil {
		return nil, err
	}
	return EnsureVolumeResponse{Handle: handle}, nil
}

func (m *Module) handleAttachVolume(ctx context.Context, cmd commands.Command) (commands.Response, error) {
	request, ok := cmd.(AttachVolumeCommand)
	if !ok {
		return nil, ErrInvalidCommand
	}
	if err := m.volumes.Attach(ctx, request.Handle, request.Opts); err != nil {
		return nil, err
	}
	return nil, nil
}

func (m *Module) handleRecordLockState(ctx context.Context, cmd commands.Command) (commands.Response, error) {
	request, ok := cmd.(RecordLockStateCommand)
	if !ok {
		return nil, ErrInvalidCommand
	}
	if err := m.setLockState(ctx, request.Locked); err != nil {
		return nil, err
	}
	m.publishLockState(request.Locked)
	return nil, nil
}

