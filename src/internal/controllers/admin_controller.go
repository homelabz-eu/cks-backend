package controllers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/homelabz-eu/cks-backend/internal/kubevirt"
	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/homelabz-eu/cks-backend/internal/sessions"
)

// AdminController handles administrative operations
type AdminController struct {
	sessionManager *sessions.SessionManager
	kubevirtClient *kubevirt.Client // ADD THIS
	logger         *logrus.Logger
}

// NewAdminController creates a new admin controller
func NewAdminController(sessionManager *sessions.SessionManager, kubevirtClient *kubevirt.Client, logger *logrus.Logger) *AdminController {
	return &AdminController{
		sessionManager: sessionManager,
		kubevirtClient: kubevirtClient, // ADD THIS
		logger:         logger,
	}
}

// RegisterRoutes registers the admin controller routes
func (ac *AdminController) RegisterRoutes(router *gin.Engine) {
	admin := router.Group("/api/v1/admin")
	{
		admin.POST("/bootstrap-pool", ac.BootstrapClusterPool)
		admin.POST("/create-snapshots", ac.CreatePoolSnapshots)
		admin.POST("/release-all-clusters", ac.ReleaseAllClusters)
		admin.GET("/clusters", ac.GetClusterPoolStatus)
		admin.GET("/sessions", ac.GetAdminSessions)
	}
}

// BootstrapClusterPool bootstraps all 3 baseline clusters
func (ac *AdminController) BootstrapClusterPool(c *gin.Context) {
	ac.logger.Info("Admin request to bootstrap cluster pool")

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Minute)
	defer cancel()

	err := ac.sessionManager.BootstrapClusterPool(ctx)
	if err != nil {
		ac.logger.WithError(err).Error("Failed to bootstrap cluster pool")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to bootstrap cluster pool",
			"details": err.Error(),
		})
		return
	}

	ac.logger.Info("Cluster pool bootstrap completed successfully")
	c.JSON(http.StatusOK, gin.H{
		"message":  "Cluster pool bootstrapped successfully",
		"clusters": []string{"cluster1", "cluster2", "cluster3"},
		"status":   "completed",
	})
}

// CreatePoolSnapshots creates snapshots from all clusters in the pool
func (ac *AdminController) CreatePoolSnapshots(c *gin.Context) {
	ac.logger.Info("Admin request to create pool snapshots")

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Minute)
	defer cancel()

	// Create snapshots for all 3 clusters
	results := make(map[string]interface{})
	clusterIDs := []string{"cluster1", "cluster2", "cluster3"}

	for _, clusterID := range clusterIDs {
		ac.logger.WithField("clusterID", clusterID).Info("Creating snapshots for cluster")

		result, err := ac.createClusterSnapshots(ctx, clusterID)
		if err != nil {
			ac.logger.WithError(err).WithField("clusterID", clusterID).Error("Failed to create snapshots")
			results[clusterID] = map[string]interface{}{
				"success": false,
				"error":   err.Error(),
			}
		} else {
			results[clusterID] = result
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Snapshot creation completed",
		"results": results,
	})
}

// ReleaseAllClusters releases all clusters in the pool
func (ac *AdminController) ReleaseAllClusters(c *gin.Context) {
	ac.logger.Info("Admin request to release all clusters")

	err := ac.sessionManager.GetClusterPool().ReleaseAllClusters()
	if err != nil {
		ac.logger.WithError(err).Error("Failed to release all clusters")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to release all clusters",
			"details": err.Error(),
		})
		return
	}

	// Get current pool status after release
	poolStats := ac.sessionManager.GetClusterPool().GetPoolStatus(false)

	ac.logger.Info("All applicable clusters released successfully")
	c.JSON(http.StatusOK, gin.H{
		"message": "All applicable clusters released successfully",
		"poolStatus": gin.H{
			"totalClusters":     poolStats.TotalClusters,
			"availableClusters": poolStats.AvailableClusters,
			"resettingClusters": poolStats.ResettingClusters,
			"lockedClusters":    poolStats.LockedClusters,
			"errorClusters":     poolStats.ErrorClusters,
			"statusByCluster":   poolStats.StatusByCluster,
		},
	})
}

// createClusterSnapshots creates snapshots for both VMs in a specific cluster
func (ac *AdminController) createClusterSnapshots(ctx context.Context, clusterID string) (map[string]interface{}, error) {
	namespace := clusterID // namespace matches clusterID
	controlPlaneVM := fmt.Sprintf("cp-%s", clusterID)
	workerVM := fmt.Sprintf("wk-%s", clusterID)

	// Generate snapshot names
	cpSnapshotName := fmt.Sprintf("cp-%s-snapshot", clusterID)
	wkSnapshotName := fmt.Sprintf("wk-%s-snapshot", clusterID)

	ac.logger.WithFields(logrus.Fields{
		"clusterID":      clusterID,
		"namespace":      namespace,
		"controlPlaneVM": controlPlaneVM,
		"workerVM":       workerVM,
		"cpSnapshot":     cpSnapshotName,
		"wkSnapshot":     wkSnapshotName,
	}).Info("Creating cluster snapshots")

	// Create control plane snapshot
	err := ac.kubevirtClient.CreateVMSnapshot(ctx, namespace, controlPlaneVM, cpSnapshotName)
	if err != nil {
		return nil, fmt.Errorf("failed to create control plane snapshot: %w", err)
	}

	// Create worker snapshot
	err = ac.kubevirtClient.CreateVMSnapshot(ctx, namespace, workerVM, wkSnapshotName)
	if err != nil {
		return nil, fmt.Errorf("failed to create worker snapshot: %w", err)
	}

	// Wait for both snapshots to be ready
	err = ac.kubevirtClient.WaitForSnapshotReady(ctx, namespace, cpSnapshotName)
	if err != nil {
		return nil, fmt.Errorf("control plane snapshot failed to become ready: %w", err)
	}

	err = ac.kubevirtClient.WaitForSnapshotReady(ctx, namespace, wkSnapshotName)
	if err != nil {
		return nil, fmt.Errorf("worker snapshot failed to become ready: %w", err)
	}

	return map[string]interface{}{
		"success": true,
		"snapshots": map[string]string{
			"controlPlane": cpSnapshotName,
			"worker":       wkSnapshotName,
		},
		"namespace": namespace,
	}, nil
}

// GetClusterPoolStatus returns detailed cluster pool status for admin dashboard
func (ac *AdminController) GetClusterPoolStatus(c *gin.Context) {
	ac.logger.Info("Admin request for cluster pool status")

	// Get detailed cluster information using existing function with detailed=true
	poolStatus := ac.sessionManager.GetClusterPool().GetPoolStatus(true)

	c.JSON(http.StatusOK, poolStatus)
}

// GetAdminSessions returns sessions list optimized for admin dashboard
func (ac *AdminController) GetAdminSessions(c *gin.Context) {
	ac.logger.Info("Admin request for sessions list")

	// Use existing ListSessions function - it already has all the data we need
	sessions := ac.sessionManager.ListSessions()

	// Return sessions with basic stats
	response := gin.H{
		"sessions": sessions,
		"stats": gin.H{
			"totalSessions": len(sessions),
			"runningSessions": func() int {
				count := 0
				for _, s := range sessions {
					if s.Status == models.SessionStatusRunning {
						count++
					}
				}
				return count
			}(),
			"provisioningSessions": func() int {
				count := 0
				for _, s := range sessions {
					if s.Status == models.SessionStatusProvisioning {
						count++
					}
				}
				return count
			}(),
		},
	}

	c.JSON(http.StatusOK, response)
}
