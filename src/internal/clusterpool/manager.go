// backend/internal/clusterpool/manager.go
package clusterpool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/homelabz-eu/cks-backend/internal/config"
	"github.com/homelabz-eu/cks-backend/internal/kubevirt"
	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	PoolSize = 3 // cluster1, cluster2, cluster3

	// Kubernetes annotations for persistent cluster state
	ClusterStatusAnnotation    = "cks.io/cluster-status"
	ClusterLastResetAnnotation = "cks.io/last-reset"
	ClusterCreatedAtAnnotation = "cks.io/created-at"
)

// Manager manages the cluster pool for session assignment
type Manager struct {
	clusters       map[string]*models.ClusterPool
	lock           sync.RWMutex
	kubeClient     kubernetes.Interface
	kubevirtClient *kubevirt.Client
	config         *config.Config
	logger         *logrus.Logger

	// Background task control
	stopCh chan struct{}
}

// NewManager creates a new cluster pool manager
func NewManager(
	cfg *config.Config,
	kubeClient kubernetes.Interface,
	kubevirtClient *kubevirt.Client,
	logger *logrus.Logger,
) (*Manager, error) {
	manager := &Manager{
		clusters:       make(map[string]*models.ClusterPool, PoolSize),
		kubeClient:     kubeClient,
		kubevirtClient: kubevirtClient,
		config:         cfg,
		logger:         logger,
		stopCh:         make(chan struct{}),
	}

	// Initialize the pool
	manager.initializePool()

	// Start background maintenance
	go manager.maintenanceLoop()

	return manager, nil
}

// initializePool reads cluster status from namespace annotations for persistence across restarts
func (m *Manager) initializePool() {
	m.logger.Info("Initializing cluster pool from namespace annotations...")

	clusterIDs := []string{"cluster1", "cluster2", "cluster3"}

	for _, clusterID := range clusterIDs {
		// Read persistent status from namespace annotation
		status := m.getClusterStatusFromNamespace(clusterID)
		lastReset := m.getLastResetFromNamespace(clusterID)

		// Use consistent naming pattern for VMs
		controlPlaneVM := fmt.Sprintf("cp-%s", clusterID)
		workerVM := fmt.Sprintf("wk-%s", clusterID)

		cluster := &models.ClusterPool{
			ClusterID:       clusterID,
			Namespace:       clusterID, // namespace matches cluster ID
			Status:          status,    // Read from namespace annotation
			ControlPlaneVM:  controlPlaneVM,
			WorkerNodeVM:    workerVM,
			CreatedAt:       time.Now(), // Could also be persisted if needed
			LastReset:       lastReset,  // Read from namespace annotation
			LastHealthCheck: time.Now(),
		}

		m.clusters[clusterID] = cluster

		m.logger.WithFields(logrus.Fields{
			"clusterID":      clusterID,
			"namespace":      cluster.Namespace,
			"controlPlaneVM": cluster.ControlPlaneVM,
			"workerVM":       cluster.WorkerNodeVM,
			"status":         cluster.Status,
			"lastReset":      cluster.LastReset,
		}).Info("Cluster initialized from persistent state")
	}

	m.logger.WithField("poolSize", len(m.clusters)).Info("Cluster pool initialized from namespace annotations")
}

// AssignCluster assigns an available cluster to a session
func (m *Manager) AssignCluster(sessionID string) (*models.ClusterPool, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	// Find first available cluster
	for clusterID, cluster := range m.clusters {
		if cluster.Status == models.StatusAvailable {
			// Lock cluster to session
			cluster.Status = models.StatusLocked
			cluster.AssignedSession = sessionID
			cluster.LockTime = time.Now()

			m.logger.WithFields(logrus.Fields{
				"clusterID": clusterID,
				"sessionID": sessionID,
			}).Info("Cluster assigned to session")

			// Return a copy to avoid external modifications
			clusterCopy := *cluster
			return &clusterCopy, nil
		}
	}

	return nil, fmt.Errorf("no available clusters in pool")
}

// ReleaseCluster releases a cluster from a session
func (m *Manager) ReleaseCluster(sessionID string) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	// Find cluster assigned to this session
	for clusterID, cluster := range m.clusters {
		if cluster.AssignedSession == sessionID {
			// Mark for reset
			cluster.Status = models.StatusResetting
			cluster.AssignedSession = ""
			cluster.LockTime = time.Time{}

			m.logger.WithFields(logrus.Fields{
				"clusterID": clusterID,
				"sessionID": sessionID,
			}).Info("Cluster released and marked for reset")

			// Trigger async reset
			go m.resetClusterAsync(clusterID)

			return nil
		}
	}

	return fmt.Errorf("no cluster found for session %s", sessionID)
}

// ReleaseAllClusters releases all clusters in the pool regardless of assignment
func (m *Manager) ReleaseAllClusters() error {

	m.lock.Lock()
	defer m.lock.Unlock()

	releasedClusters := make([]string, 0)

	// Release all clusters in the pool
	for clusterID, cluster := range m.clusters {
		// Only release if currently locked or in error state

		// Mark for reset
		cluster.Status = models.StatusResetting
		cluster.AssignedSession = ""
		cluster.LockTime = time.Time{}

		releasedClusters = append(releasedClusters, clusterID)

		m.logger.WithFields(logrus.Fields{
			"clusterID": clusterID,
			"previousStatus": func() string {
				if cluster.AssignedSession != "" {
					return fmt.Sprintf("locked to session %s", cluster.AssignedSession)
				}
				return "error state"
			}(),
		}).Info("Cluster released and marked for reset")

		// Trigger async reset
		go m.resetClusterAsync(clusterID)

	}

	if len(releasedClusters) == 0 {
		m.logger.Info("No clusters needed to be released - all already available")
		return nil
	}

	m.logger.WithFields(logrus.Fields{
		"releasedClusters": releasedClusters,
		"totalReleased":    len(releasedClusters),
	}).Info("Released all applicable clusters - cleanup will remove old restore PVCs during reset")

	return nil
}

// GetPoolStatus returns current pool statistics with optional detailed cluster information
func (m *Manager) GetPoolStatus(detailed ...bool) *models.ClusterPoolStats {
	m.lock.RLock()
	defer m.lock.RUnlock()

	isDetailed := len(detailed) > 0 && detailed[0]

	stats := &models.ClusterPoolStats{
		TotalClusters:   len(m.clusters),
		StatusByCluster: make(map[string]models.ClusterStatus),
	}

	// Initialize DetailedClusters if detailed info is requested
	if isDetailed {
		stats.DetailedClusters = make(map[string]*models.ClusterPool)
	}

	for clusterID, cluster := range m.clusters {
		stats.StatusByCluster[clusterID] = cluster.Status

		// Add detailed cluster info if requested
		if isDetailed {
			clusterCopy := *cluster
			stats.DetailedClusters[clusterID] = &clusterCopy
		}

		switch cluster.Status {
		case models.StatusAvailable:
			stats.AvailableClusters++
		case models.StatusLocked:
			stats.LockedClusters++
		case models.StatusResetting:
			stats.ResettingClusters++
		case models.StatusError:
			stats.ErrorClusters++
		}
	}

	return stats
}

// GetClusterByID returns a cluster by ID
func (m *Manager) GetClusterByID(clusterID string) (*models.ClusterPool, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	cluster, exists := m.clusters[clusterID]
	if !exists {
		return nil, fmt.Errorf("cluster %s not found", clusterID)
	}

	// Return a copy
	clusterCopy := *cluster
	return &clusterCopy, nil
}

// MarkClusterAvailable marks a cluster as available after bootstrap
func (m *Manager) MarkClusterAvailable(clusterID string) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	cluster, exists := m.clusters[clusterID]
	if !exists {
		return fmt.Errorf("cluster %s not found", clusterID)
	}

	cluster.Status = models.StatusAvailable

	// Persist to namespace annotation
	err := m.updateClusterStatusInNamespace(clusterID, models.StatusAvailable)
	if err != nil {
		m.logger.WithError(err).WithField("clusterID", clusterID).Error("Failed to persist cluster status to namespace")
		// Continue anyway - in-memory state is updated
	}

	m.logger.WithField("clusterID", clusterID).Info("Cluster marked as available and persisted")
	return nil
}

// resetClusterAsync performs cluster reset in background using snapshots
func (m *Manager) resetClusterAsync(clusterID string) {
	m.logger.WithField("clusterID", clusterID).Info("Starting cluster reset from snapshots with cleanup")

	// Mark as resetting and persist
	m.lock.Lock()
	if cluster, exists := m.clusters[clusterID]; exists {
		cluster.Status = models.StatusResetting
		m.updateClusterStatusInNamespace(clusterID, models.StatusResetting)
	}
	m.lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute) // Increased timeout for cleanup
	defer cancel()

	m.lock.RLock()
	cluster, exists := m.clusters[clusterID]
	m.lock.RUnlock()

	if !exists {
		m.logger.WithField("clusterID", clusterID).Error("Cluster not found for reset")
		return
	}

	// Step 1: Stop the VMs first
	m.logger.WithField("clusterID", clusterID).Info("Stopping VMs before cleanup and restore")
	err := m.kubevirtClient.StopVMs(ctx, cluster.Namespace, cluster.ControlPlaneVM, cluster.WorkerNodeVM)
	if err != nil {
		m.logger.WithError(err).WithField("clusterID", clusterID).Warn("Failed to stop VMs, continuing with reset")
	}

	// Step 2: Clean up old restore objects and PVCs
	m.logger.WithField("clusterID", clusterID).Info("Cleaning up old restore objects and PVCs")
	err = m.kubevirtClient.CleanupOldRestores(ctx, cluster.Namespace, cluster.ControlPlaneVM, cluster.WorkerNodeVM)
	if err != nil {
		m.logger.WithError(err).WithField("clusterID", clusterID).Warn("Cleanup of old restores failed, continuing with restore")
		// Don't fail the reset process due to cleanup issues
	}

	// Wait a bit for cleanup to complete
	time.Sleep(10 * time.Second)

	// Generate snapshot names (matching the pattern from snapshot creation)
	cpSnapshotName := fmt.Sprintf("cp-%s-snapshot", clusterID)
	wkSnapshotName := fmt.Sprintf("wk-%s-snapshot", clusterID)

	// Step 3: Restore control plane VM from snapshot
	m.logger.WithField("clusterID", clusterID).Info("Restoring control plane VM from snapshot")
	err = m.kubevirtClient.RestoreVMFromSnapshot(ctx, cluster.Namespace, cluster.ControlPlaneVM, cpSnapshotName)
	if err != nil {
		m.logger.WithError(err).WithField("clusterID", clusterID).Error("Failed to restore control plane VM")
		m.markClusterError(clusterID, err)
		return
	}

	// Step 4: Restore worker VM from snapshot
	m.logger.WithField("clusterID", clusterID).Info("Restoring worker VM from snapshot")
	err = m.kubevirtClient.RestoreVMFromSnapshot(ctx, cluster.Namespace, cluster.WorkerNodeVM, wkSnapshotName)
	if err != nil {
		m.logger.WithError(err).WithField("clusterID", clusterID).Error("Failed to restore worker VM")
		m.markClusterError(clusterID, err)
		return
	}

	// Mark cluster as available and persist
	m.lock.Lock()
	if cluster, exists := m.clusters[clusterID]; exists {
		cluster.Status = models.StatusAvailable
		cluster.LastReset = time.Now()
		m.updateClusterStatusInNamespace(clusterID, models.StatusAvailable)
	}
	m.lock.Unlock()

	m.logger.WithField("clusterID", clusterID).Info("Cluster reset completed successfully with cleanup")
}

// markClusterError marks a cluster as in error state
func (m *Manager) markClusterError(clusterID string, err error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if cluster, exists := m.clusters[clusterID]; exists {
		cluster.Status = models.StatusError

		// Persist error state
		persistErr := m.updateClusterStatusInNamespace(clusterID, models.StatusError)
		if persistErr != nil {
			m.logger.WithError(persistErr).Error("Failed to persist error status")
		}

		m.logger.WithError(err).WithField("clusterID", clusterID).Error("Cluster marked as error and persisted")
	}
}

// maintenanceLoop performs periodic maintenance tasks
func (m *Manager) maintenanceLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.performMaintenance()
		case <-m.stopCh:
			return
		}
	}
}

// performMaintenance checks cluster health and performs cleanup
func (m *Manager) performMaintenance() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.logger.Debug("Performing cluster pool maintenance")

	for clusterID, cluster := range m.clusters {
		cluster.LastHealthCheck = time.Now()

		// TODO: Add actual health checks in later phases
		m.logger.WithFields(logrus.Fields{
			"clusterID":       clusterID,
			"status":          cluster.Status,
			"assignedSession": cluster.AssignedSession,
		}).Debug("Cluster maintenance check")
	}
}

// Stop gracefully shuts down the cluster pool manager
func (m *Manager) Stop() {
	close(m.stopCh)
	m.logger.Info("Cluster pool manager stopped")
}

// getClusterStatusFromNamespace reads status from namespace annotation
func (m *Manager) getClusterStatusFromNamespace(clusterID string) models.ClusterStatus {
	ctx := context.Background()

	ns, err := m.kubeClient.CoreV1().Namespaces().Get(ctx, clusterID, metav1.GetOptions{})
	if err != nil {
		m.logger.WithError(err).WithField("clusterID", clusterID).Warn("Failed to get namespace, defaulting to creating")
		return models.StatusCreating
	}

	if ns.Annotations == nil {
		return models.StatusCreating
	}

	statusStr, exists := ns.Annotations[ClusterStatusAnnotation]
	if !exists {
		return models.StatusCreating
	}

	return models.ClusterStatus(statusStr)
}

// getLastResetFromNamespace reads last reset time from namespace annotation
func (m *Manager) getLastResetFromNamespace(clusterID string) time.Time {
	ctx := context.Background()

	ns, err := m.kubeClient.CoreV1().Namespaces().Get(ctx, clusterID, metav1.GetOptions{})
	if err != nil {
		return time.Now()
	}

	if ns.Annotations == nil {
		return time.Now()
	}

	resetTimeStr, exists := ns.Annotations[ClusterLastResetAnnotation]
	if !exists {
		return time.Now()
	}

	resetTime, err := time.Parse(time.RFC3339, resetTimeStr)
	if err != nil {
		return time.Now()
	}

	return resetTime
}

// updateClusterStatusInNamespace persists status to namespace annotation
func (m *Manager) updateClusterStatusInNamespace(clusterID string, status models.ClusterStatus) error {
	ctx := context.Background()

	ns, err := m.kubeClient.CoreV1().Namespaces().Get(ctx, clusterID, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", clusterID, err)
	}

	if ns.Annotations == nil {
		ns.Annotations = make(map[string]string)
	}

	ns.Annotations[ClusterStatusAnnotation] = string(status)
	ns.Annotations[ClusterLastResetAnnotation] = time.Now().Format(time.RFC3339)

	_, err = m.kubeClient.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update namespace %s: %w", clusterID, err)
	}

	m.logger.WithFields(logrus.Fields{
		"clusterID": clusterID,
		"status":    status,
	}).Debug("Updated cluster status in namespace annotation")

	return nil
}
