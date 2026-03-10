// backend/internal/controllers/session_controller.go

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/homelabz-eu/cks-backend/internal/services"
	"github.com/homelabz-eu/cks-backend/internal/validation"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// SessionController handles HTTP requests related to sessions
type SessionController struct {
	sessionService   services.SessionService
	scenarioService  services.ScenarioService
	logger           *logrus.Logger
	unifiedValidator *validation.UnifiedValidator
}

// NewSessionController creates a new session controller
func NewSessionController(sessionService services.SessionService, scenarioService services.ScenarioService, logger *logrus.Logger, unifiedValidator *validation.UnifiedValidator) *SessionController {
	return &SessionController{
		sessionService:   sessionService,
		scenarioService:  scenarioService,
		logger:           logger,
		unifiedValidator: unifiedValidator,
	}
}

// RegisterRoutes registers the session controller routes
func (sc *SessionController) RegisterRoutes(router *gin.Engine) {
	sessions := router.Group("/api/v1/sessions")
	{
		sessions.POST("", sc.CreateSession)
		sessions.GET("", sc.ListSessions)
		sessions.GET("/:id", sc.GetSession)
		sessions.DELETE("/:id", sc.DeleteSession)
		sessions.PUT("/:id/extend", sc.ExtendSession)
		sessions.GET("/:id/tasks", sc.ListTasks)
		sessions.POST("/:id/tasks/:taskId/validate", sc.ValidateTask)
	}
}

// CreateSession handles the creation of a new session
func (sc *SessionController) CreateSession(c *gin.Context) {
	var request models.CreateSessionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	// Create a timeout context
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Create session
	session, err := sc.sessionService.CreateSession(ctx, request.ScenarioID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to create session: %v", err)})
		return
	}

	c.JSON(http.StatusCreated, models.CreateSessionResponse{
		SessionID: session.ID,
		Status:    string(session.Status),
	})
}

// ListSessions returns a list of all active sessions
func (sc *SessionController) ListSessions(c *gin.Context) {
	sessions := sc.sessionService.ListSessions()
	c.JSON(http.StatusOK, sessions)
}

// GetSession returns details for a specific session
func (sc *SessionController) GetSession(c *gin.Context) {
	sessionID := c.Param("id")

	session, err := sc.sessionService.GetSession(sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Session not found: %v", err)})
		return
	}

	// Add additional status check for VM readiness
	if session.Status == models.SessionStatusProvisioning {
		// Check VMs status
		vmStatus, err := sc.sessionService.CheckVMsStatus(c.Request.Context(), session)
		if err != nil {
			// Just log the error, don't fail the request
			sc.logger.WithError(err).WithField("sessionID", sessionID).Warn("Failed to check VM status")
		} else if vmStatus == "Running" {
			// Update session status to running if VMs are ready
			sc.sessionService.UpdateSessionStatus(sessionID, models.SessionStatusRunning, "")
			session.Status = models.SessionStatusRunning
		}
	}

	c.JSON(http.StatusOK, session)
}

// DeleteSession deletes a session and its resources
func (sc *SessionController) DeleteSession(c *gin.Context) {
	sessionID := c.Param("id")

	// Create a timeout context
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	err := sc.sessionService.DeleteSession(ctx, sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to delete session: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Session deleted successfully"})
}

// ExtendSession extends the expiration time of a session
func (sc *SessionController) ExtendSession(c *gin.Context) {
	sessionID := c.Param("id")

	// Default extension is 30 minutes
	extension := 30 * time.Minute

	// Check for custom extension time
	type ExtendRequest struct {
		Minutes int `json:"minutes"`
	}

	var request ExtendRequest
	if c.ShouldBindJSON(&request) == nil && request.Minutes > 0 {
		extension = time.Duration(request.Minutes) * time.Minute
	}

	err := sc.sessionService.ExtendSession(sessionID, extension)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to extend session: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Session extended successfully"})
}

// ListTasks lists the tasks for a session
func (sc *SessionController) ListTasks(c *gin.Context) {
	sessionID := c.Param("id")

	// Get session to access its tasks
	session, err := sc.sessionService.GetSession(sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Session not found: %v", err)})
		return
	}

	c.JSON(http.StatusOK, session.Tasks)
}

// ValidateTask validates a specific task in a session
// ValidateTask validates a specific task in a session using unified validator
func (sc *SessionController) ValidateTask(c *gin.Context) {
	sessionID := c.Param("id")
	taskID := c.Param("taskId")

	sc.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"taskID":    taskID,
	}).Info("Starting unified task validation")

	// Get session
	session, err := sc.sessionService.GetSession(sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Session not found: %v", err)})
		return
	}

	// Get task validation rules
	task, err := sc.getTaskWithValidationRules(session.ScenarioID, taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// Check if task has validation rules
	if len(task.Validation) == 0 {
		// Return success for tasks without validation rules
		response := &validation.ValidationResponse{
			Success:   true,
			Message:   "No validation rules defined for this task",
			Results:   []validation.ValidationResult{},
			Timestamp: time.Now(),
		}
		c.JSON(http.StatusOK, response)
		return
	}

	// Use unified validator
	ctx, cancel := context.WithTimeout(c.Request.Context(), 300*time.Second)
	defer cancel()

	validationResponse, err := sc.unifiedValidator.ValidateTask(ctx, session, task.Validation)
	if err != nil {
		sc.logger.WithError(err).Error("Unified validation failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Validation failed: %v", err)})
		return
	}

	// Update session with results (simplified)
	sc.updateSessionTaskStatus(sessionID, taskID, validationResponse)

	sc.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"taskID":    taskID,
		"success":   validationResponse.Success,
		"results":   len(validationResponse.Results),
	}).Info("Sending validation response")

	c.JSON(http.StatusOK, validationResponse)
}

// Helper method to get task with validation rules
func (sc *SessionController) getTaskWithValidationRules(scenarioID, taskID string) (*models.Task, error) {
	if scenarioID == "" {
		return nil, fmt.Errorf("session has no associated scenario")
	}

	// Get scenario using the scenario service
	scenario, err := sc.scenarioService.GetScenario(scenarioID)
	if err != nil {
		return nil, fmt.Errorf("failed to load scenario %s: %w", scenarioID, err)
	}

	// Find the specific task
	for i, task := range scenario.Tasks {
		if task.ID == taskID {
			sc.logger.WithFields(logrus.Fields{
				"scenarioID":      scenarioID,
				"taskID":          taskID,
				"validationRules": len(task.Validation),
				"taskTitle":       task.Title,
			}).Debug("Found task with validation rules")

			return &scenario.Tasks[i], nil
		}
	}

	return nil, fmt.Errorf("task %s not found in scenario %s", taskID, scenarioID)
}

// Helper method to update session task status
func (sc *SessionController) updateSessionTaskStatus(sessionID, taskID string, response *validation.ValidationResponse) {
	status := "failed"
	if response.Success {
		status = "completed"
	}

	// Use existing session service to update task status
	err := sc.sessionService.UpdateTaskStatus(sessionID, taskID, status)
	if err != nil {
		sc.logger.WithError(err).WithFields(logrus.Fields{
			"sessionID": sessionID,
			"taskID":    taskID,
		}).Error("Failed to update task status")
	}
}
