// backend/internal/services/session_service.go

package services

import (
	"context"
	"time"

	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/homelabz-eu/cks-backend/internal/sessions"
	"github.com/homelabz-eu/cks-backend/internal/validation"
)

// SessionServiceImpl implements the SessionService interface
type SessionServiceImpl struct {
	sessionManager *sessions.SessionManager
}

// NewSessionService creates a new session service
func NewSessionService(sessionManager *sessions.SessionManager) SessionService {
	return &SessionServiceImpl{
		sessionManager: sessionManager,
	}
}

// CreateSession creates a new session
func (s *SessionServiceImpl) CreateSession(ctx context.Context, scenarioID string) (*models.Session, error) {
	return s.sessionManager.CreateSession(ctx, scenarioID)
}

// GetSession returns a session by ID
func (s *SessionServiceImpl) GetSession(sessionID string) (*models.Session, error) {
	return s.sessionManager.GetSession(sessionID)
}

// ListSessions returns all sessions
func (s *SessionServiceImpl) ListSessions() []*models.Session {
	return s.sessionManager.ListSessions()
}

// DeleteSession deletes a session
func (s *SessionServiceImpl) DeleteSession(ctx context.Context, sessionID string) error {
	return s.sessionManager.DeleteSession(ctx, sessionID)
}

// ExtendSession extends the session expiration time
func (s *SessionServiceImpl) ExtendSession(sessionID string, duration time.Duration) error {
	return s.sessionManager.ExtendSession(sessionID, duration)
}

// UpdateTaskStatus updates the status of a task
func (s *SessionServiceImpl) UpdateTaskStatus(sessionID, taskID string, status string) error {
	return s.sessionManager.UpdateTaskStatus(sessionID, taskID, status)
}

// ValidateTask validates a task
func (s *SessionServiceImpl) ValidateTask(ctx context.Context, sessionID, taskID string) (*validation.ValidationResponse, error) {
	return s.sessionManager.ValidateTask(ctx, sessionID, taskID)
}

// CheckVMsStatus checks the status of VMs
func (s *SessionServiceImpl) CheckVMsStatus(ctx context.Context, session *models.Session) (string, error) {
	return s.sessionManager.CheckVMsStatus(ctx, session)
}

// UpdateSessionStatus updates the session status
func (s *SessionServiceImpl) UpdateSessionStatus(sessionID string, status models.SessionStatus, message string) error {
	return s.sessionManager.UpdateSessionStatus(sessionID, status, message)
}

// RegisterTerminalSession registers a terminal session
func (s *SessionServiceImpl) RegisterTerminalSession(sessionID, terminalID, target string) error {
	return s.sessionManager.RegisterTerminalSession(sessionID, terminalID, target)
}

// UnregisterTerminalSession unregisters a terminal session
func (s *SessionServiceImpl) UnregisterTerminalSession(sessionID, terminalID string) error {
	return s.sessionManager.UnregisterTerminalSession(sessionID, terminalID)
}

// GetOrCreateTerminalSession gets existing terminal or creates new one
func (s *SessionServiceImpl) GetOrCreateTerminalSession(sessionID, target string) (string, bool, error) {
	return s.sessionManager.GetOrCreateTerminalSession(sessionID, target)
}

// StoreTerminalSession stores terminal info in session
func (s *SessionServiceImpl) StoreTerminalSession(sessionID, terminalID, target string) error {
	return s.sessionManager.StoreTerminalSession(sessionID, terminalID, target)
}

// MarkTerminalInactive marks a terminal as inactive
func (s *SessionServiceImpl) MarkTerminalInactive(sessionID, terminalID string) error {
	return s.sessionManager.MarkTerminalInactive(sessionID, terminalID)
}
