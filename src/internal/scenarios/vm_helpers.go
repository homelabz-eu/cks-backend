// backend/internal/scenarios/vm_helpers.go

package scenarios

import (
	"fmt"

	"github.com/homelabz-eu/cks-backend/internal/models"
)

// VMTarget represents the target VM for operations
type VMTarget string

const (
	VMTargetControlPlane VMTarget = "control-plane"
	VMTargetWorker       VMTarget = "worker"
	VMTargetBoth         VMTarget = "both"
)

// VMHelper provides utility functions for VM operations
type VMHelper struct{}

func NewVMHelper() *VMHelper {
	return &VMHelper{}
}

// GetTargetVMs returns the appropriate VMs based on the target specification
func (h *VMHelper) GetTargetVMs(session *models.Session, target string) []string {
	switch VMTarget(target) {
	case VMTargetControlPlane:
		return []string{session.ControlPlaneVM}
	case VMTargetWorker:
		return []string{session.WorkerNodeVM}
	case VMTargetBoth:
		return []string{session.ControlPlaneVM, session.WorkerNodeVM}
	default:
		// Default to control plane if target is not recognized
		return []string{session.ControlPlaneVM}
	}
}

// GetTargetVM returns a single VM based on the target specification
func (h *VMHelper) GetTargetVM(session *models.Session, target string) string {
	vms := h.GetTargetVMs(session, target)
	if len(vms) > 0 {
		return vms[0]
	}
	return session.ControlPlaneVM
}

// ValidateTarget checks if a target specification is valid
func (h *VMHelper) ValidateTarget(target string) error {
	switch VMTarget(target) {
	case VMTargetControlPlane, VMTargetWorker, VMTargetBoth:
		return nil
	default:
		return fmt.Errorf("invalid VM target: %s (must be 'control-plane', 'worker', or 'both')", target)
	}
}

// GetVMDisplayName returns a human-readable name for a VM
func (h *VMHelper) GetVMDisplayName(session *models.Session, vmName string) string {
	switch vmName {
	case session.ControlPlaneVM:
		return "Control Plane"
	case session.WorkerNodeVM:
		return "Worker Node"
	default:
		return vmName
	}
}

// IsControlPlane checks if a VM is the control plane
func (h *VMHelper) IsControlPlane(session *models.Session, vmName string) bool {
	return vmName == session.ControlPlaneVM
}

// IsWorker checks if a VM is a worker node
func (h *VMHelper) IsWorker(session *models.Session, vmName string) bool {
	return vmName == session.WorkerNodeVM
}
