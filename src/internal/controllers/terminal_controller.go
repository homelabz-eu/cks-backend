package controllers

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/homelabz-eu/cks-backend/internal/services"
)

type TerminalController struct {
	terminalService services.TerminalService
	sessionService  services.SessionService
	logger          *logrus.Logger
}

func NewTerminalController(
	terminalService services.TerminalService,
	sessionService services.SessionService,
	logger *logrus.Logger,
) *TerminalController {
	return &TerminalController{
		terminalService: terminalService,
		sessionService:  sessionService,
		logger:          logger,
	}
}

func (tc *TerminalController) RegisterRoutes(router *gin.Engine) {
	router.POST("/api/v1/sessions/:id/terminals", tc.CreateTerminal)
}

func (tc *TerminalController) CreateTerminal(c *gin.Context) {
	sessionID := c.Param("id")

	var request models.CreateTerminalRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		tc.logger.WithError(err).Error("Invalid terminal creation request")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	session, err := tc.sessionService.GetSession(sessionID)
	if err != nil {
		tc.logger.WithError(err).WithField("sessionID", sessionID).Error("Session not found")
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Session not found: %v", err)})
		return
	}

	if session.Status != models.SessionStatusRunning {
		tc.logger.WithFields(logrus.Fields{
			"sessionID": sessionID,
			"status":    session.Status,
		}).Warn("Attempted to create terminal for non-running session")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Session is not ready yet, current status: %s", session.Status),
		})
		return
	}

	targetVM := ""
	switch request.Target {
	case "control-plane":
		targetVM = session.ControlPlaneVM
	case "worker-node":
		targetVM = session.WorkerNodeVM
	default:
		tc.logger.WithField("target", request.Target).Error("Invalid terminal target")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid terminal target"})
		return
	}

	terminalURL, err := tc.terminalService.GetTerminalURL(c.Request.Context(), session.Namespace, targetVM)
	if err != nil {
		tc.logger.WithError(err).Error("Failed to get terminal URL")
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to get terminal URL: %v", err)})
		return
	}

	tc.logger.WithFields(logrus.Fields{
		"sessionID":   sessionID,
		"target":      request.Target,
		"terminalURL": terminalURL,
	}).Info("Terminal URL generated")

	c.JSON(http.StatusOK, models.CreateTerminalResponse{
		TerminalURL: terminalURL,
	})
}
