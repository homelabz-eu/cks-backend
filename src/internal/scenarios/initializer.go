package scenarios

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/homelabz-eu/cks-backend/internal/kubevirt"
	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"
)

type ScenarioInitializer struct {
	kubeClient     kubernetes.Interface
	kubevirtClient *kubevirt.Client
	logger         *logrus.Logger
}

func NewScenarioInitializer(kubeClient kubernetes.Interface, kubevirtClient *kubevirt.Client, logger *logrus.Logger) *ScenarioInitializer {
	return &ScenarioInitializer{
		kubeClient:     kubeClient,
		kubevirtClient: kubevirtClient,
		logger:         logger,
	}
}

func (si *ScenarioInitializer) InitializeScenario(ctx context.Context, session *models.Session, scenario *models.Scenario) error {
	si.logger.WithFields(logrus.Fields{
		"sessionID":  session.ID,
		"scenarioID": scenario.ID,
	}).Info("Starting scenario initialization")

	// Load setup configuration
	setupSteps, err := si.loadSetupSteps(scenario)
	if err != nil {
		return fmt.Errorf("failed to load setup steps: %w", err)
	}

	// Execute each setup step
	for i, step := range setupSteps {
		si.logger.WithField("step", step.ID).Infof("Executing setup step %d/%d", i+1, len(setupSteps))

		err := si.executeSetupStep(ctx, session, step)
		if err != nil {
			// Retry logic
			for retry := 0; retry < step.RetryCount; retry++ {
				si.logger.WithError(err).Warnf("Setup step failed, retry %d/%d", retry+1, step.RetryCount)
				time.Sleep(5 * time.Second)

				err = si.executeSetupStep(ctx, session, step)
				if err == nil {
					break
				}
			}

			if err != nil {
				return fmt.Errorf("setup step %s failed: %w", step.ID, err)
			}
		}

		// Wait for conditions
		if len(step.Conditions) > 0 {
			err = si.waitForConditions(ctx, session, step)
			if err != nil {
				return fmt.Errorf("conditions not met for step %s: %w", step.ID, err)
			}
		}
	}

	si.logger.WithField("sessionID", session.ID).Info("Scenario initialization completed")
	return nil
}

func (si *ScenarioInitializer) executeSetupStep(ctx context.Context, session *models.Session, step models.SetupStep) error {
	switch step.Type {
	case "command":
		return si.executeCommand(ctx, session, step)
	case "resource":
		return si.createResource(ctx, session, step)
	case "script":
		return si.executeScript(ctx, session, step)
	case "wait":
		return si.waitForDuration(ctx, step)
	default:
		return fmt.Errorf("unknown setup step type: %s", step.Type)
	}
}

func (si *ScenarioInitializer) executeCommand(ctx context.Context, session *models.Session, step models.SetupStep) error {
	targets := si.getTargetVMs(session, step.Target)

	for _, target := range targets {
		output, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, step.Command)
		if err != nil {
			return fmt.Errorf("command execution failed on %s: %w", target, err)
		}

		si.logger.WithFields(logrus.Fields{
			"target": target,
			"output": output,
		}).Debug("Command executed")
	}

	return nil
}

func (si *ScenarioInitializer) createResource(ctx context.Context, session *models.Session, step models.SetupStep) error {
	// Apply the resource YAML using kubectl
	tempFile := fmt.Sprintf("/tmp/resource-%s-%s.yaml", session.ID, step.ID)

	// Write resource content to temp file in VM
	cmd := fmt.Sprintf("cat > %s << 'EOF'\n%s\nEOF", tempFile, step.Resource)
	_, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, cmd)
	if err != nil {
		return fmt.Errorf("failed to create resource file: %w", err)
	}

	// Apply the resource
	applyCmd := fmt.Sprintf("kubectl apply -f %s", tempFile)
	output, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, applyCmd)
	if err != nil {
		return fmt.Errorf("failed to apply resource: %w", err)
	}

	si.logger.WithField("output", output).Debug("Resource created")

	// Cleanup temp file
	si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, fmt.Sprintf("rm %s", tempFile))

	return nil
}

func (si *ScenarioInitializer) waitForConditions(ctx context.Context, session *models.Session, step models.SetupStep) error {
	timeout := step.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for conditions")
		case <-ticker.C:
			allMet := true

			for _, condition := range step.Conditions {
				met, err := si.checkCondition(ctx, session, condition)
				if err != nil {
					si.logger.WithError(err).Warn("Error checking condition")
					allMet = false
					break
				}

				if !met {
					allMet = false
					break
				}
			}

			if allMet {
				return nil
			}
		}
	}
}

func (si *ScenarioInitializer) getTargetVMs(session *models.Session, target string) []string {
	switch target {
	case "control-plane":
		return []string{session.ControlPlaneVM}
	case "worker":
		return []string{session.WorkerNodeVM}
	case "both":
		return []string{session.ControlPlaneVM, session.WorkerNodeVM}
	default:
		return []string{session.ControlPlaneVM}
	}
}

// Add these methods to ScenarioInitializer

func (si *ScenarioInitializer) loadSetupSteps(scenario *models.Scenario) ([]models.SetupStep, error) {
	// If setup steps are already in the scenario, return them
	if len(scenario.SetupSteps) > 0 {
		return scenario.SetupSteps, nil
	}

	// Otherwise load from setup directory
	scenarioPath := filepath.Join("scenarios", scenario.ID)
	setupFile := filepath.Join(scenarioPath, "setup", "init.yaml")

	if _, err := os.Stat(setupFile); os.IsNotExist(err) {
		// No setup file, that's okay
		return []models.SetupStep{}, nil
	}

	content, err := os.ReadFile(setupFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read setup file: %w", err)
	}

	var setup struct {
		Steps []models.SetupStep `yaml:"steps"`
	}

	err = yaml.Unmarshal(content, &setup)
	if err != nil {
		return nil, fmt.Errorf("failed to parse setup file: %w", err)
	}

	return setup.Steps, nil
}

func (si *ScenarioInitializer) executeScript(ctx context.Context, session *models.Session, step models.SetupStep) error {
	// Determine target VM
	targets := si.getTargetVMs(session, step.Target)

	for _, target := range targets {
		// Create temporary script file
		scriptFile := fmt.Sprintf("/tmp/setup-%s-%s.sh", session.ID, step.ID)

		// Write script to file
		cmd := fmt.Sprintf("cat > %s << 'EOF'\n%s\nEOF\nchmod +x %s", scriptFile, step.Script, scriptFile)
		_, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, cmd)
		if err != nil {
			return fmt.Errorf("failed to create script file: %w", err)
		}

		// Execute script
		output, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, fmt.Sprintf("bash %s", scriptFile))
		if err != nil {
			return fmt.Errorf("script execution failed: %w, output: %s", err, output)
		}

		// Cleanup
		si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, fmt.Sprintf("rm %s", scriptFile))

		si.logger.WithFields(logrus.Fields{
			"target": target,
			"output": output,
		}).Debug("Script executed")
	}

	return nil
}

func (si *ScenarioInitializer) waitForDuration(ctx context.Context, step models.SetupStep) error {
	si.logger.WithField("duration", step.Timeout).Info("Waiting for duration")

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(step.Timeout):
		return nil
	}
}

func (si *ScenarioInitializer) checkCondition(ctx context.Context, session *models.Session, condition models.SetupCondition) (bool, error) {
	switch condition.Type {
	case "resource_exists":
		cmd := fmt.Sprintf("kubectl get %s", condition.Resource)
		output, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, cmd)
		if err != nil || strings.Contains(output, "NotFound") {
			return false, nil
		}
		return true, nil

	case "command_success":
		output, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, condition.Command)
		if err != nil {
			si.logger.WithError(err).WithField("output", output).Debug("Command failed")
			return false, nil
		}
		return true, nil

	case "pod_ready":
		// Example: check if a pod is ready
		parts := strings.Split(condition.Resource, "/")
		if len(parts) != 2 {
			return false, fmt.Errorf("invalid resource format for pod_ready: %s", condition.Resource)
		}

		namespace := parts[0]
		podName := parts[1]

		cmd := fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'", podName, namespace)
		output, err := si.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, cmd)
		if err != nil {
			return false, nil
		}

		return strings.TrimSpace(output) == "True", nil

	default:
		return false, fmt.Errorf("unknown condition type: %s", condition.Type)
	}
}
