package validation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/homelabz-eu/cks-backend/internal/kubevirt"
	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/sirupsen/logrus"
)

// UnifiedValidator handles all validation logic in a single, clean interface
type UnifiedValidator struct {
	kubevirtClient *kubevirt.Client
	logger         *logrus.Logger
}

// ValidationRequest represents a complete validation request
type ValidationRequest struct {
	SessionID string                  `json:"sessionId"`
	TaskID    string                  `json:"taskId"`
	Rules     []models.ValidationRule `json:"rules"`
}

// ValidationResponse provides a clean, consistent response format
type ValidationResponse struct {
	Success   bool               `json:"success"`
	Message   string             `json:"message"`
	Results   []ValidationResult `json:"results"`
	Timestamp time.Time          `json:"timestamp"`
}

// ValidationResult represents a single rule validation result
type ValidationResult struct {
	RuleID      string      `json:"ruleId"`
	RuleType    string      `json:"ruleType"`
	Passed      bool        `json:"passed"`
	Message     string      `json:"message"`
	Expected    interface{} `json:"expected,omitempty"`
	Actual      interface{} `json:"actual,omitempty"`
	ErrorCode   string      `json:"errorCode,omitempty"`
	Description string      `json:"description,omitempty"`
}

// NewUnifiedValidator creates a new validation service
func NewUnifiedValidator(kubevirtClient *kubevirt.Client, logger *logrus.Logger) *UnifiedValidator {
	return &UnifiedValidator{
		kubevirtClient: kubevirtClient,
		logger:         logger,
	}
}

// ValidateTask performs all validations for a task and returns clean results
func (uv *UnifiedValidator) ValidateTask(ctx context.Context, session *models.Session, rules []models.ValidationRule) (*ValidationResponse, error) {
	response := &ValidationResponse{
		Success:   true,
		Message:   "All validations passed",
		Results:   make([]ValidationResult, 0, len(rules)),
		Timestamp: time.Now(),
	}

	uv.logger.WithFields(logrus.Fields{
		"sessionID": session.ID,
		"taskRules": len(rules),
	}).Info("Starting unified task validation")

	// Process each validation rule
	for _, rule := range rules {
		result := uv.validateRule(ctx, session, rule)
		response.Results = append(response.Results, result)

		if !result.Passed {
			response.Success = false
			response.Message = "One or more validations failed"
		}
	}

	uv.logger.WithFields(logrus.Fields{
		"sessionID": session.ID,
		"success":   response.Success,
		"results":   len(response.Results),
	}).Info("Unified validation completed")

	return response, nil
}

// validateRule processes a single validation rule with clean error handling
func (uv *UnifiedValidator) validateRule(ctx context.Context, session *models.Session, rule models.ValidationRule) ValidationResult {
	result := ValidationResult{
		RuleID:      rule.ID,
		RuleType:    rule.Type,
		Passed:      false,
		Description: rule.Description,
	}

	uv.logger.WithFields(logrus.Fields{
		"ruleID":   rule.ID,
		"ruleType": rule.Type,
		"session":  session.ID,
	}).Debug("Processing validation rule")

	// Route to appropriate validator based on rule type
	switch rule.Type {
	case "resource_exists":
		uv.validateResourceExists(ctx, session, rule, &result)
	case "command":
		uv.validateCommand(ctx, session, rule, &result)
	case "script":
		uv.validateScript(ctx, session, rule, &result)
	case "file_exists":
		uv.validateFileExists(ctx, session, rule, &result)
	case "file_content":
		uv.validateFileContent(ctx, session, rule, &result)
	default:
		result.Message = fmt.Sprintf("Unknown validation type: %s", rule.Type)
		result.ErrorCode = "UNKNOWN_VALIDATION_TYPE"
	}

	uv.logger.WithFields(logrus.Fields{
		"ruleID":  rule.ID,
		"passed":  result.Passed,
		"message": result.Message,
	}).Debug("Rule validation completed")

	return result
}

// validateResourceExists checks if a Kubernetes resource exists
func (uv *UnifiedValidator) validateResourceExists(ctx context.Context, session *models.Session, rule models.ValidationRule, result *ValidationResult) {
	if rule.Resource == nil {
		result.Message = "Resource specification is missing"
		result.ErrorCode = "MISSING_RESOURCE_SPEC"
		return
	}

	namespace := rule.Resource.Namespace
	if namespace == "" {
		namespace = "default"
	}

	cmd := fmt.Sprintf("kubectl get %s %s -n %s",
		strings.ToLower(rule.Resource.Kind),
		rule.Resource.Name,
		namespace)

	output, err := uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, session.ControlPlaneVM, cmd, false)

	if err != nil || strings.Contains(output, "NotFound") || strings.Contains(output, "Error") {
		result.Message = fmt.Sprintf("%s '%s' does not exist in namespace '%s'",
			rule.Resource.Kind, rule.Resource.Name, namespace)
		result.ErrorCode = "RESOURCE_NOT_FOUND"
		result.Expected = "Resource should exist"
		result.Actual = "Resource not found"
		return
	}

	result.Passed = true
	result.Message = fmt.Sprintf("%s '%s' exists in namespace '%s'",
		rule.Resource.Kind, rule.Resource.Name, namespace)
}

// validateCommand executes a command and validates the result
func (uv *UnifiedValidator) validateCommand(ctx context.Context, session *models.Session, rule models.ValidationRule, result *ValidationResult) {
	if rule.Command == nil {
		result.Message = "Command specification is missing"
		result.ErrorCode = "MISSING_COMMAND_SPEC"
		return
	}

	// Determine target VM
	target := session.ControlPlaneVM
	if rule.Command.Target == "worker" {
		target = session.WorkerNodeVM
	}

	output, err := uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, rule.Command.Command, false)
	result.Actual = strings.TrimSpace(output)

	switch rule.Condition {
	case "success":
		if err == nil {
			result.Passed = true
			result.Message = "Command executed successfully"
		} else {
			result.Message = "Command execution failed"
			result.ErrorCode = "COMMAND_FAILED"
		}

	case "output_equals":
		expectedOutput := fmt.Sprintf("%v", rule.Value)
		result.Expected = expectedOutput

		if err == nil && strings.TrimSpace(output) == strings.TrimSpace(expectedOutput) {
			result.Passed = true
			result.Message = "Command output matches expected value"
		} else {
			result.Message = fmt.Sprintf("Expected output '%s', got '%s'", expectedOutput, strings.TrimSpace(output))
			result.ErrorCode = "OUTPUT_MISMATCH"
		}

	default:
		result.Message = fmt.Sprintf("Unknown condition: %s", rule.Condition)
		result.ErrorCode = "UNKNOWN_CONDITION"
	}
}

// validateScript executes a script and validates the result
func (uv *UnifiedValidator) validateScript(ctx context.Context, session *models.Session, rule models.ValidationRule, result *ValidationResult) {
	if rule.Script == nil {
		result.Message = "Script specification is missing"
		result.ErrorCode = "MISSING_SCRIPT_SPEC"
		return
	}

	// Determine target VM
	target := session.ControlPlaneVM
	if rule.Script.Target == "worker" {
		target = session.WorkerNodeVM
	}

	// Create a temporary script file
	scriptFile := fmt.Sprintf("/tmp/validation-%s-%s.sh", session.ID, rule.ID)

	// Write script to file
	cmd := fmt.Sprintf("cat > %s << 'EOF'\n%s\nEOF && chmod +x %s", scriptFile, rule.Script.Script, scriptFile)
	_, err := uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, cmd, false)
	if err != nil {
		result.Message = fmt.Sprintf("Failed to create script: %v", err)
		result.ErrorCode = "SCRIPT_CREATION_FAILED"
		return
	}

	// Execute script and check exit code
	scriptCmd := fmt.Sprintf("bash %s; echo $?", scriptFile)
	output, err := uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, scriptCmd, false)

	// Cleanup
	uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, fmt.Sprintf("rm %s", scriptFile), false)

	if err != nil {
		result.Message = fmt.Sprintf("Script execution failed: %v", err)
		result.ErrorCode = "SCRIPT_EXECUTION_FAILED"
		return
	}

	// Extract exit code from output
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		result.Message = "Failed to get script exit code"
		result.ErrorCode = "NO_EXIT_CODE"
		return
	}

	exitCodeStr := lines[len(lines)-1]
	exitCode := 0
	if _, err := fmt.Sscanf(exitCodeStr, "%d", &exitCode); err != nil {
		result.Message = fmt.Sprintf("Failed to parse exit code: %v", err)
		result.ErrorCode = "INVALID_EXIT_CODE"
		return
	}

	// Check if exit code matches expected
	expectedCode := 0
	if rule.Script.SuccessCode != 0 {
		expectedCode = rule.Script.SuccessCode
	}

	result.Expected = expectedCode
	result.Actual = exitCode

	if exitCode == expectedCode {
		result.Passed = true
		result.Message = "Script validation passed"
	} else {
		result.Message = fmt.Sprintf("Script failed with exit code %d, expected %d", exitCode, expectedCode)
		result.ErrorCode = "SCRIPT_EXIT_CODE_MISMATCH"
	}
}

// validateFileExists checks if a file exists on the target VM
func (uv *UnifiedValidator) validateFileExists(ctx context.Context, session *models.Session, rule models.ValidationRule, result *ValidationResult) {
	if rule.File == nil {
		result.Message = "File specification is missing"
		result.ErrorCode = "MISSING_FILE_SPEC"
		return
	}

	// Determine target VM
	target := session.ControlPlaneVM
	if rule.File.Target == "worker" {
		target = session.WorkerNodeVM
	}

	// Check if file exists
	cmd := fmt.Sprintf("test -f %s", rule.File.Path)
	_, err := uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, cmd, false)

	result.Expected = "File should exist"
	result.Actual = fmt.Sprintf("File %s", rule.File.Path)

	if err == nil {
		result.Passed = true
		result.Message = fmt.Sprintf("File %s exists", rule.File.Path)
	} else {
		result.Message = fmt.Sprintf("File %s does not exist", rule.File.Path)
		result.ErrorCode = "FILE_NOT_FOUND"
	}
}

// validateFileContent checks the content of a file
func (uv *UnifiedValidator) validateFileContent(ctx context.Context, session *models.Session, rule models.ValidationRule, result *ValidationResult) {
	if rule.File == nil {
		result.Message = "File specification is missing"
		result.ErrorCode = "MISSING_FILE_SPEC"
		return
	}

	// Determine target VM
	target := session.ControlPlaneVM
	if rule.File.Target == "worker" {
		target = session.WorkerNodeVM
	}

	// Get file content
	cmd := fmt.Sprintf("cat %s", rule.File.Path)
	output, err := uv.kubevirtClient.ExecuteCommandInVM(ctx, session.Namespace, target, cmd, false)

	if err != nil {
		result.Message = fmt.Sprintf("Failed to read file %s: %v", rule.File.Path, err)
		result.ErrorCode = "FILE_READ_FAILED"
		return
	}

	result.Actual = strings.TrimSpace(output)

	// Check condition
	switch rule.Condition {
	case "contains":
		expectedValue := fmt.Sprintf("%v", rule.Value)
		result.Expected = fmt.Sprintf("File should contain '%s'", expectedValue)

		if strings.Contains(output, expectedValue) {
			result.Passed = true
			result.Message = fmt.Sprintf("File contains expected content")
		} else {
			result.Message = fmt.Sprintf("File does not contain '%s'", expectedValue)
			result.ErrorCode = "CONTENT_NOT_FOUND"
		}

	default:
		result.Message = fmt.Sprintf("Unknown condition: %s", rule.Condition)
		result.ErrorCode = "UNKNOWN_CONDITION"
	}
}
