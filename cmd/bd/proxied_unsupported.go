package main

import (
	"errors"
	"fmt"
)

// Capability names a class of operation that cannot be serviced in
// proxied-server mode. These are the genuinely-impossible raw-Dolt operations
// (version-control surgery, compaction) — NOT the work surface, which the
// routed store services normally. The const block is the single source of
// truth that tests reference by exact value.
type Capability string

const (
	// CapabilityDoltPush is `bd dolt push` — pushing the local Dolt history to
	// a remote requires direct repository access, not a multiplexed proxy.
	CapabilityDoltPush Capability = "dolt-push"
	// CapabilityDoltPull is `bd dolt pull`.
	CapabilityDoltPull Capability = "dolt-pull"
	// CapabilityDoltCommit is `bd dolt commit` and other raw history writes.
	CapabilityDoltCommit Capability = "dolt-commit"
	// CapabilityDoltConflicts is `bd dolt conflicts list|resolve` — inspecting and
	// resolving merge conflicts in the working set via raw DOLT_CONFLICTS_RESOLVE.
	CapabilityDoltConflicts Capability = "dolt-conflicts"
	// CapabilityCompaction is `bd compact` — rewriting Dolt history requires
	// exclusive direct access to the underlying repository.
	CapabilityCompaction Capability = "compaction"
	// CapabilityFederation is the federation staging-branch surgery surface.
	CapabilityFederation Capability = "federation"
	// CapabilityRemoteSync is RemoteStore.Push/Pull/ForcePush — the
	// DoltRemoteUseCase covers remote *config* CRUD only, not actual sync.
	CapabilityRemoteSync Capability = "remote-sync"
)

// UnsupportedInProxiedModeError reports that a command was invoked in
// proxied-server mode for an operation that mode cannot service. It is
// errors.As-checkable so callers and tests can assert the exact capability,
// and it carries an actionable message — never a silent empty-JSON exit 0,
// which would read as success and strand the operation (the dead-drop bug).
type UnsupportedInProxiedModeError struct {
	Capability Capability
}

// Error implements the error interface with an operator-actionable message.
func (e *UnsupportedInProxiedModeError) Error() string {
	return fmt.Sprintf(
		"%s requires direct ServerMode and is not supported in proxied-server mode; set [beads] proxied=false to use it",
		e.Capability)
}

// ErrUnsupportedInProxiedMode constructs the typed error for a capability.
func ErrUnsupportedInProxiedMode(c Capability) error {
	return &UnsupportedInProxiedModeError{Capability: c}
}

// AsUnsupportedInProxiedMode reports whether err is an
// UnsupportedInProxiedModeError, returning it for inspection.
func AsUnsupportedInProxiedMode(err error) (*UnsupportedInProxiedModeError, bool) {
	var target *UnsupportedInProxiedModeError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// guardUnsupportedInProxiedMode exits with the typed-error message when running
// in proxied-server mode. Call it at the top of a command that genuinely cannot
// be serviced through the proxy. It exits non-zero (never empty-JSON + exit 0),
// which is the contract the dead-drop guard test enforces.
func guardUnsupportedInProxiedMode(c Capability) {
	if !proxiedServerMode {
		return
	}
	FatalErrorRespectJSON("%s", ErrUnsupportedInProxiedMode(c).Error())
}
