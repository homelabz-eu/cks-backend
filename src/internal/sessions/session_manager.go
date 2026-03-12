package sessions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/homelabz-eu/cks-backend/internal/clusterpool"
	"github.com/homelabz-eu/cks-backend/internal/config"
	"github.com/homelabz-eu/cks-backend/internal/kubevirt"
	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/homelabz-eu/cks-backend/internal/scenarios"
	"github.com/homelabz-eu/cks-backend/internal/validation"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

type SessionManager struct {
	store            SessionStore
	clientset        *kubernetes.Clientset
	kubevirtClient   *kubevirt.Client
	config           *config.Config
	unifiedValidator *validation.UnifiedValidator
	logger           *logrus.Logger
	stopCh           chan struct{}
	scenarioManager  *scenarios.ScenarioManager
	clusterPool      *clusterpool.Manager
}

func NewSessionManager(
	cfg *config.Config,
	clientset *kubernetes.Clientset,
	kubevirtClient *kubevirt.Client,
	unifiedValidator *validation.UnifiedValidator,
	logger *logrus.Logger,
	scenarioManager *scenarios.ScenarioManager,
	clusterPool *clusterpool.Manager,
	store SessionStore,
) (*SessionManager, error) {
	sm := &SessionManager{
		store:            store,
		clientset:        clientset,
		kubevirtClient:   kubevirtClient,
		config:           cfg,
		unifiedValidator: unifiedValidator,
		logger:           logger,
		stopCh:           make(chan struct{}),
		scenarioManager:  scenarioManager,
		clusterPool:      clusterPool,
	}

	sm.cleanStaleTerminals()

	go sm.cleanupExpiredSessions()

	return sm, nil
}

func (sm *SessionManager) CreateSession(ctx context.Context, scenarioID string) (*models.Session, error) {
	count, err := sm.store.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to count sessions: %w", err)
	}
	if count >= sm.config.MaxConcurrentSessions {
		return nil, fmt.Errorf("maximum number of concurrent sessions reached")
	}

	sessionID := uuid.New().String()[:8]

	assignedCluster, err := sm.clusterPool.AssignCluster(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to assign cluster: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"clusterID": assignedCluster.ClusterID,
		"namespace": assignedCluster.Namespace,
	}).Info("Cluster assigned to session")

	var tasks []models.TaskStatus
	var scenarioTitle string

	if scenarioID != "" {
		scenario, err := sm.loadScenario(ctx, scenarioID)
		if err != nil {
			sm.clusterPool.ReleaseCluster(sessionID)
			return nil, fmt.Errorf("failed to load scenario: %w", err)
		}

		scenarioTitle = scenario.Title

		tasks = make([]models.TaskStatus, 0, len(scenario.Tasks))
		for _, task := range scenario.Tasks {
			tasks = append(tasks, models.TaskStatus{
				ID:     task.ID,
				Status: "pending",
			})
		}

		sm.logger.WithFields(logrus.Fields{
			"sessionID":     sessionID,
			"scenarioID":    scenarioID,
			"scenarioTitle": scenarioTitle,
			"taskCount":     len(tasks),
		}).Info("Initialized session with scenario tasks")
	}

	session := &models.Session{
		ID:               sessionID,
		Namespace:        assignedCluster.Namespace,
		ScenarioID:       scenarioID,
		Status:           models.SessionStatusRunning,
		StartTime:        time.Now(),
		ExpirationTime:   time.Now().Add(time.Duration(sm.config.SessionTimeoutMinutes) * time.Minute),
		ControlPlaneVM:   assignedCluster.ControlPlaneVM,
		WorkerNodeVM:     assignedCluster.WorkerNodeVM,
		Tasks:            tasks,
		TerminalSessions: make(map[string]string),
		ActiveTerminals:  make(map[string]models.TerminalInfo),
		AssignedCluster:  assignedCluster.ClusterID,
		ClusterLockTime:  assignedCluster.LockTime,
	}

	if err := sm.store.Save(ctx, session); err != nil {
		sm.clusterPool.ReleaseCluster(sessionID)
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":      sessionID,
		"clusterID":      assignedCluster.ClusterID,
		"namespace":      session.Namespace,
		"scenarioID":     scenarioID,
		"scenarioTitle":  scenarioTitle,
		"controlPlaneVM": session.ControlPlaneVM,
		"workerNodeVM":   session.WorkerNodeVM,
	}).Info("Session created with assigned cluster - ready immediately")

	if scenarioID != "" {
		go func() {
			initCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			err := sm.initializeScenario(initCtx, session)
			if err != nil {
				sm.logger.WithError(err).WithField("sessionID", sessionID).Error("Failed to initialize scenario (session still usable)")
			}
		}()
	}

	return session, nil
}

func (sm *SessionManager) GetSession(sessionID string) (*models.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return sm.store.Get(ctx, sessionID)
}

func (sm *SessionManager) ListSessions() []*models.Session {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessions, err := sm.store.List(ctx)
	if err != nil {
		sm.logger.WithError(err).Error("Failed to list sessions")
		return []*models.Session{}
	}

	return sessions
}

func (sm *SessionManager) DeleteSession(ctx context.Context, sessionID string) error {
	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if err := sm.store.Delete(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"clusterID": session.AssignedCluster,
	}).Info("Deleting session and releasing cluster")

	if session.AssignedCluster != "" {
		err := sm.clusterPool.ReleaseCluster(sessionID)
		if err != nil {
			sm.logger.WithError(err).WithFields(logrus.Fields{
				"sessionID": sessionID,
				"clusterID": session.AssignedCluster,
			}).Error("Failed to release cluster")
		}
	}
	return nil
}

func (sm *SessionManager) ExtendSession(sessionID string, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	session.ExpirationTime = time.Now().Add(duration)

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":      sessionID,
		"expirationTime": session.ExpirationTime,
	}).Info("Session extended")

	return nil
}

func (sm *SessionManager) UpdateTaskStatus(sessionID, taskID string, status string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	found := false
	for i, task := range session.Tasks {
		if task.ID == taskID {
			session.Tasks[i].Status = status
			session.Tasks[i].ValidationTime = time.Now()
			found = true
			break
		}
	}

	if !found {
		session.Tasks = append(session.Tasks, models.TaskStatus{
			ID:             taskID,
			Status:         status,
			ValidationTime: time.Now(),
		})
	}

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"taskID":    taskID,
		"status":    status,
	}).Info("Task status updated")

	return nil
}

func (sm *SessionManager) ValidateTask(ctx context.Context, sessionID, taskID string) (*validation.ValidationResponse, error) {
	session, err := sm.GetSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	if session.ScenarioID == "" {
		return nil, fmt.Errorf("session has no associated scenario")
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":  sessionID,
		"taskID":     taskID,
		"scenarioID": session.ScenarioID,
	}).Debug("Starting task validation")

	scenario, err := sm.loadScenario(ctx, session.ScenarioID)
	if err != nil {
		return nil, fmt.Errorf("failed to load scenario: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"scenarioID": scenario.ID,
		"taskCount":  len(scenario.Tasks),
		"tasks": func() []map[string]interface{} {
			taskInfo := make([]map[string]interface{}, len(scenario.Tasks))
			for i, t := range scenario.Tasks {
				taskInfo[i] = map[string]interface{}{
					"id":              t.ID,
					"title":           t.Title,
					"validationCount": len(t.Validation),
				}
			}
			return taskInfo
		}(),
	}).Debug("Loaded scenario for validation with task details")

	var taskToValidate *models.Task
	for i, task := range scenario.Tasks {
		sm.logger.WithFields(logrus.Fields{
			"checkingTaskID":  task.ID,
			"targetTaskID":    taskID,
			"taskTitle":       task.Title,
			"validationCount": len(task.Validation),
			"match":           task.ID == taskID,
		}).Debug("Checking task match")

		if task.ID == taskID {
			taskToValidate = &scenario.Tasks[i]
			sm.logger.WithFields(logrus.Fields{
				"taskID":    taskID,
				"foundTask": true,
				"validationRules": func() []map[string]interface{} {
					rules := make([]map[string]interface{}, len(task.Validation))
					for j, rule := range task.Validation {
						rules[j] = map[string]interface{}{
							"id":   rule.ID,
							"type": rule.Type,
						}
					}
					return rules
				}(),
			}).Debug("Found task with validation rules")
			break
		}
	}

	if taskToValidate == nil {
		sm.logger.WithFields(logrus.Fields{
			"sessionID":  sessionID,
			"taskID":     taskID,
			"scenarioID": session.ScenarioID,
			"availableTasks": func() []string {
				ids := make([]string, len(scenario.Tasks))
				for i, t := range scenario.Tasks {
					ids[i] = t.ID
				}
				return ids
			}(),
		}).Error("Task not found in scenario")

		return nil, fmt.Errorf("task %s not found in scenario %s", taskID, session.ScenarioID)
	}

	sm.logger.WithFields(logrus.Fields{
		"taskID":          taskID,
		"taskTitle":       taskToValidate.Title,
		"validationRules": len(taskToValidate.Validation),
	}).Info("Found task for validation")

	if len(taskToValidate.Validation) == 0 {
		sm.logger.WithFields(logrus.Fields{
			"sessionID":  sessionID,
			"taskID":     taskID,
			"scenarioID": session.ScenarioID,
		}).Warn("Task has no validation rules")

		return &validation.ValidationResponse{
			Success:   true,
			Message:   "No validation rules defined for this task",
			Results:   []validation.ValidationResult{},
			Timestamp: time.Now(),
		}, nil
	}

	for i, rule := range taskToValidate.Validation {
		sm.logger.WithFields(logrus.Fields{
			"taskID":    taskID,
			"ruleIndex": i,
			"ruleID":    rule.ID,
			"ruleType":  rule.Type,
		}).Debug("Validating rule")
	}

	result, err := sm.unifiedValidator.ValidateTask(ctx, session, taskToValidate.Validation)
	if err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	status := "failed"
	if result.Success {
		status = "completed"
	}

	err = sm.UpdateTaskValidationResult(sessionID, taskID, status, result)
	if err != nil {
		sm.logger.WithError(err).WithFields(logrus.Fields{
			"sessionID": sessionID,
			"taskID":    taskID,
			"status":    status,
		}).Error("Failed to update task validation result")
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"taskID":    taskID,
		"success":   result.Success,
		"status":    status,
		"details":   len(result.Results),
	}).Info("Task validation completed")

	return result, nil
}

func (sm *SessionManager) UpdateTaskValidationResult(sessionID, taskID string, status string, validationResult *validation.ValidationResponse) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	found := false
	for i, task := range session.Tasks {
		if task.ID == taskID {
			session.Tasks[i].Status = status
			session.Tasks[i].ValidationTime = time.Now()
			session.Tasks[i].ValidationResult = &models.ValidationResponseRef{
				Success:   validationResult.Success,
				Message:   validationResult.Message,
				Timestamp: time.Now(),
			}
			found = true
			break
		}
	}

	if !found {
		session.Tasks = append(session.Tasks, models.TaskStatus{
			ID:             taskID,
			Status:         status,
			ValidationTime: time.Now(),
			ValidationResult: &models.ValidationResponseRef{
				Success:   validationResult.Success,
				Message:   validationResult.Message,
				Timestamp: time.Now(),
			},
		})
	}

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"taskID":    taskID,
		"status":    status,
		"success":   validationResult.Success,
	}).Info("Task validation result stored in session")

	return nil
}

func (sm *SessionManager) RegisterTerminalSession(sessionID, terminalID, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if session.TerminalSessions == nil {
		session.TerminalSessions = make(map[string]string)
	}

	session.TerminalSessions[terminalID] = target

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":  sessionID,
		"terminalID": terminalID,
		"target":     target,
	}).Debug("Terminal session registered")

	return nil
}

func (sm *SessionManager) UnregisterTerminalSession(sessionID, terminalID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if session.TerminalSessions == nil {
		return nil
	}

	delete(session.TerminalSessions, terminalID)

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":  sessionID,
		"terminalID": terminalID,
	}).Debug("Terminal session unregistered")

	return nil
}

func (sm *SessionManager) createNamespace(ctx context.Context, namespace string) error {
	sm.logger.WithField("namespace", namespace).Info("Creating namespace")

	_, err := sm.clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		sm.logger.WithField("namespace", namespace).Info("Namespace already exists, continuing")
		return nil
	}

	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing namespace: %w", err)
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
			Labels: map[string]string{
				"cks.io/session": "true",
			},
		},
	}

	_, err = sm.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			sm.logger.WithField("namespace", namespace).Info("Namespace created by another process, continuing")
			return nil
		}
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	sm.logger.WithField("namespace", namespace).Info("Namespace created successfully")
	return nil
}

func (sm *SessionManager) setupResourceQuotas(ctx context.Context, namespace string) error {
	sm.logger.WithField("namespace", namespace).Info("Setting up resource quotas")

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "session-quota",
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("20"),
			},
		},
	}

	existingQuota, err := sm.clientset.CoreV1().ResourceQuotas(namespace).Get(ctx, "session-quota", metav1.GetOptions{})
	if err == nil {
		existingQuota.Spec = quota.Spec
		_, err = sm.clientset.CoreV1().ResourceQuotas(namespace).Update(ctx, existingQuota, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update resource quota: %w", err)
		}
		sm.logger.WithField("namespace", namespace).Info("Resource quota updated")
		return nil
	}

	if errors.IsNotFound(err) {
		_, err = sm.clientset.CoreV1().ResourceQuotas(namespace).Create(ctx, quota, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create resource quota: %w", err)
		}
		sm.logger.WithField("namespace", namespace).Info("Resource quota created")
		return nil
	}

	return fmt.Errorf("failed to check existing quota: %w", err)
}

func (sm *SessionManager) loadScenario(ctx context.Context, scenarioID string) (*models.Scenario, error) {
	return sm.scenarioManager.GetScenario(scenarioID)
}

func (sm *SessionManager) initializeScenario(ctx context.Context, session *models.Session) error {
	if session.ScenarioID == "" {
		return fmt.Errorf("session has no scenario ID")
	}

	scenario, err := sm.scenarioManager.GetScenario(session.ScenarioID)
	if err != nil {
		return fmt.Errorf("failed to load scenario: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":     session.ID,
		"scenarioID":    scenario.ID,
		"scenarioTitle": scenario.Title,
		"setupSteps":    len(scenario.SetupSteps),
	}).Info("Initializing scenario for session")

	if len(scenario.SetupSteps) == 0 {
		sm.logger.WithField("scenarioID", scenario.ID).Debug("No setup steps for scenario")
		return nil
	}

	initializer := scenarios.NewScenarioInitializer(sm.clientset, sm.kubevirtClient, sm.logger)

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	err = initializer.InitializeScenario(initCtx, session, scenario)
	if err != nil {
		return fmt.Errorf("scenario initialization failed: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":  session.ID,
		"scenarioID": scenario.ID,
	}).Info("Scenario initialization completed")

	return nil
}

func (sm *SessionManager) GetSessionWithScenario(ctx context.Context, sessionID string) (*models.Session, *models.Scenario, error) {
	session, err := sm.GetSession(sessionID)
	if err != nil {
		return nil, nil, err
	}

	if session.ScenarioID == "" {
		return session, nil, nil
	}

	scenario, err := sm.loadScenario(ctx, session.ScenarioID)
	if err != nil {
		sm.logger.WithError(err).WithField("scenarioID", session.ScenarioID).Warn("Failed to load scenario for session")
		return session, nil, nil
	}

	return session, scenario, nil
}

func (sm *SessionManager) cleanupExpiredSessions() {
	ticker := time.NewTicker(time.Duration(sm.config.CleanupIntervalMinutes) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sm.logger.Debug("Running session cleanup")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

			sessions, err := sm.store.List(ctx)
			if err != nil {
				sm.logger.WithError(err).Error("Failed to list sessions for cleanup")
				cancel()
				continue
			}

			now := time.Now()
			for _, session := range sessions {
				if now.After(session.ExpirationTime) && session.Status != models.SessionStatusFailed {
					sm.logger.WithField("sessionID", session.ID).Info("Cleaning up expired session")

					session.Status = models.SessionStatusFailed
					session.StatusMessage = "Session expired"
					sm.store.Save(ctx, session)

					err := sm.DeleteSession(ctx, session.ID)
					if err != nil {
						sm.logger.WithError(err).WithField("sessionID", session.ID).Error("Error cleaning up expired session environment")
					}

					sm.logger.WithField("sessionID", session.ID).Info("Expired session removed")
				}
			}

			cancel()

		case <-sm.stopCh:
			return
		}
	}
}

func (sm *SessionManager) Stop() {
	close(sm.stopCh)
	sm.logger.Info("Session manager stopped")
}

func (sm *SessionManager) CheckVMsStatus(ctx context.Context, session *models.Session) (string, error) {
	controlPlaneStatus, err := sm.kubevirtClient.GetVMStatus(ctx, session.Namespace, session.ControlPlaneVM)
	if err != nil {
		return "", fmt.Errorf("failed to get control plane VM status: %w", err)
	}

	workerNodeStatus, err := sm.kubevirtClient.GetVMStatus(ctx, session.Namespace, session.WorkerNodeVM)
	if err != nil {
		return "", fmt.Errorf("failed to get worker node VM status: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":          session.ID,
		"controlPlaneStatus": controlPlaneStatus,
		"workerNodeStatus":   workerNodeStatus,
	}).Debug("VM status check")

	if controlPlaneStatus == "Running" && workerNodeStatus == "Running" {
		if session.AssignedCluster != "" {
			sm.logger.WithField("sessionID", session.ID).Debug("Checking SSH readiness for cluster pool VMs")

			sshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			cpSSHReady, _ := sm.kubevirtClient.IsVMSSHReady(sshCtx, session.Namespace, session.ControlPlaneVM)
			workerSSHReady, _ := sm.kubevirtClient.IsVMSSHReady(sshCtx, session.Namespace, session.WorkerNodeVM)

			sm.logger.WithFields(logrus.Fields{
				"sessionID":      session.ID,
				"cpSSHReady":     cpSSHReady,
				"workerSSHReady": workerSSHReady,
			}).Debug("SSH readiness check completed")

			if cpSSHReady && workerSSHReady {
				return "Running", nil
			}

			return "Starting", nil
		}

		return "Running", nil
	}

	return controlPlaneStatus, nil
}

func (sm *SessionManager) UpdateSessionStatus(sessionID string, status models.SessionStatus, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	session.Status = status
	session.StatusMessage = message

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID": sessionID,
		"status":    status,
		"message":   message,
	}).Info("Session status updated")

	return nil
}

func (sm *SessionManager) GetOrCreateTerminalSession(sessionID, target string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("session not found: %s", sessionID)
	}

	expectedTerminalID := fmt.Sprintf("%s-%s", sessionID, target)

	if session.ActiveTerminals == nil {
		session.ActiveTerminals = make(map[string]models.TerminalInfo)
	}

	if terminalInfo, exists := session.ActiveTerminals[expectedTerminalID]; exists {
		if terminalInfo.Status == "active" {
			terminalInfo.LastUsedAt = time.Now()
			session.ActiveTerminals[expectedTerminalID] = terminalInfo
			sm.store.Save(ctx, session)

			sm.logger.WithFields(logrus.Fields{
				"sessionID":  sessionID,
				"terminalID": expectedTerminalID,
				"target":     target,
			}).Info("Reusing existing terminal session with deterministic ID")

			return expectedTerminalID, true, nil
		}
	}

	return expectedTerminalID, false, nil
}

func (sm *SessionManager) StoreTerminalSession(sessionID, terminalID, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	expectedTerminalID := fmt.Sprintf("%s-%s", sessionID, target)
	if terminalID != expectedTerminalID {
		sm.logger.WithFields(logrus.Fields{
			"sessionID":          sessionID,
			"providedTerminalID": terminalID,
			"expectedTerminalID": expectedTerminalID,
			"target":             target,
		}).Warn("Terminal ID mismatch - using expected deterministic ID")
		terminalID = expectedTerminalID
	}

	if session.ActiveTerminals == nil {
		session.ActiveTerminals = make(map[string]models.TerminalInfo)
	}

	session.ActiveTerminals[terminalID] = models.TerminalInfo{
		ID:         terminalID,
		Target:     target,
		Status:     "active",
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}

	if session.TerminalSessions == nil {
		session.TerminalSessions = make(map[string]string)
	}
	session.TerminalSessions[terminalID] = target

	if err := sm.store.Save(ctx, session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.logger.WithFields(logrus.Fields{
		"sessionID":  sessionID,
		"terminalID": terminalID,
		"target":     target,
	}).Info("Stored terminal session info with deterministic ID")

	return nil
}

func (sm *SessionManager) MarkTerminalInactive(sessionID, terminalID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := sm.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if session.ActiveTerminals != nil {
		if terminalInfo, exists := session.ActiveTerminals[terminalID]; exists {
			terminalInfo.Status = "disconnected"
			terminalInfo.LastUsedAt = time.Now()
			session.ActiveTerminals[terminalID] = terminalInfo

			if err := sm.store.Save(ctx, session); err != nil {
				return fmt.Errorf("failed to save session: %w", err)
			}
		}
	}

	return nil
}

// CLUSTER POOL

func (sm *SessionManager) BootstrapClusterPool(ctx context.Context) error {
	clusterIDs := []string{"cluster1", "cluster2", "cluster3"}

	sm.logger.Info("Starting cluster pool bootstrap")

	for _, clusterID := range clusterIDs {
		sm.logger.WithField("clusterID", clusterID).Info("Starting bootstrap for cluster")

		err := sm.bootstrapClusterInNamespace(ctx, clusterID)
		if err != nil {
			return fmt.Errorf("failed to bootstrap cluster %s: %w", clusterID, err)
		}

		sm.logger.WithField("clusterID", clusterID).Info("Cluster bootstrap completed")

		time.Sleep(30 * time.Second)
	}

	sm.logger.Info("All clusters bootstrapped successfully")
	return nil
}

func (sm *SessionManager) BootstrapSingleCluster(ctx context.Context, clusterID string) error {
	validClusters := map[string]bool{"cluster1": true, "cluster2": true, "cluster3": true}
	if !validClusters[clusterID] {
		return fmt.Errorf("invalid cluster ID: %s", clusterID)
	}
	return sm.bootstrapClusterInNamespace(ctx, clusterID)
}

func (sm *SessionManager) bootstrapClusterInNamespace(ctx context.Context, clusterID string) error {
	namespace := clusterID

	sm.logger.WithField("clusterID", clusterID).Info("Bootstrapping cluster")

	controlPlaneVM := fmt.Sprintf("cp-%s", clusterID)
	workerNodeVM := fmt.Sprintf("wk-%s", clusterID)

	session := &models.Session{
		ID:             clusterID,
		Namespace:      namespace,
		Status:         models.SessionStatusProvisioning,
		ControlPlaneVM: controlPlaneVM,
		WorkerNodeVM:   workerNodeVM,
		StartTime:      time.Now(),
		ExpirationTime: time.Now().Add(240 * time.Hour),
	}

	err := sm.cleanupExistingCluster(ctx, session)
	if err != nil {
		sm.logger.WithError(err).WithField("clusterID", clusterID).Warn("Failed to cleanup existing cluster, continuing...")
	}

	err = sm.provisionFromBootstrapForClusterPool(ctx, session)
	if err != nil {
		return fmt.Errorf("failed to bootstrap cluster %s: %w", clusterID, err)
	}

	sm.logger.WithField("clusterID", clusterID).Info("Cluster bootstrap completed")

	err = sm.clusterPool.MarkClusterAvailable(clusterID)
	if err != nil {
		sm.logger.WithError(err).WithField("clusterID", clusterID).Error("Failed to mark cluster as available")
		return fmt.Errorf("failed to mark cluster available: %w", err)
	}

	return nil
}

func (sm *SessionManager) cleanupExistingCluster(ctx context.Context, session *models.Session) error {
	sm.logger.WithField("namespace", session.Namespace).Info("Cleaning up existing cluster resources")

	err := sm.kubevirtClient.DeleteVMs(ctx, session.Namespace, session.ControlPlaneVM, session.WorkerNodeVM)
	if err != nil {
		sm.logger.WithError(err).Warn("Failed to delete existing VMs")
	}

	time.Sleep(10 * time.Second)

	return nil
}

func (sm *SessionManager) provisionFromBootstrapForClusterPool(ctx context.Context, session *models.Session) error {
	sm.logger.WithField("clusterID", session.ID).Info("Provisioning cluster for pool using bootstrap method")

	err := sm.kubevirtClient.VerifyKubeVirtAvailable(ctx)
	if err != nil {
		sm.logger.WithError(err).Error("Failed to verify KubeVirt availability")
		return fmt.Errorf("failed to verify KubeVirt availability: %w", err)
	}

	namespaceCtx, cancelNamespace := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelNamespace()
	err = sm.createNamespace(namespaceCtx, session.Namespace)
	if err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	time.Sleep(2 * time.Second)

	quotaCtx, cancelQuota := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelQuota()
	sm.logger.WithField("namespace", session.Namespace).Info("Setting up resource quotas")
	err = sm.setupResourceQuotas(quotaCtx, session.Namespace)
	if err != nil {
		return fmt.Errorf("failed to set up resource quotas: %w", err)
	}

	time.Sleep(2 * time.Second)

	vmCtx, cancelVM := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelVM()
	sm.logger.WithField("clusterID", session.ID).Info("Creating KubeVirt VMs")
	err = sm.kubevirtClient.CreateCluster(vmCtx, session.Namespace, session.ControlPlaneVM, session.WorkerNodeVM)
	if err != nil {
		return fmt.Errorf("failed to create VMs: %w", err)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, 15*time.Minute)
	defer cancelWait()
	sm.logger.WithField("clusterID", session.ID).Info("Waiting for VMs to be ready")
	err = sm.kubevirtClient.WaitForVMsReady(waitCtx, session.Namespace, session.ControlPlaneVM, session.WorkerNodeVM)
	if err != nil {
		return fmt.Errorf("failed waiting for VMs: %w", err)
	}

	sm.logger.WithField("clusterID", session.ID).Info("Cluster pool bootstrap completed successfully")
	return nil
}

func (sm *SessionManager) cleanStaleTerminals() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sm.logger.Info("Checking for stale terminals on startup")

	sessions, err := sm.store.List(ctx)
	if err != nil {
		sm.logger.WithError(err).Warn("Failed to list sessions for stale terminal cleanup")
		return
	}

	for _, session := range sessions {
		if session.ActiveTerminals != nil {
			modified := false
			for terminalID, terminalInfo := range session.ActiveTerminals {
				if terminalInfo.Status == "disconnected" &&
					time.Since(terminalInfo.LastUsedAt) > 5*time.Minute {
					delete(session.ActiveTerminals, terminalID)
					modified = true
					sm.logger.WithFields(logrus.Fields{
						"sessionID":  session.ID,
						"terminalID": terminalID,
					}).Info("Removed stale disconnected terminal")
				}
			}
			if modified {
				sm.store.Save(ctx, session)
			}
		}
	}
}

func (sm *SessionManager) GetClusterPool() *clusterpool.Manager {
	return sm.clusterPool
}
