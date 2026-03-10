package kubevirt

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"kubevirt.io/client-go/kubecli"

	"github.com/homelabz-eu/cks/backend/internal/config"
	"github.com/sirupsen/logrus"
	snapshotv1beta1 "kubevirt.io/api/snapshot/v1beta1"
)

// Client represents a KubeVirt client for managing VMs
type Client struct {
	kubeClient        kubernetes.Interface
	virtClient        kubecli.KubevirtClient
	config            *config.Config
	restConfig        *rest.Config
	templateCache     map[string]*template.Template
	logger            *logrus.Logger
	kubernetesContext string
	tempKubeconfigPath string // Cached path to temporary kubeconfig with correct context
}

// Retry configuration constants
const (
	DefaultMaxRetries   = 3
	DefaultRetryDelay   = 10 * time.Second
	DefaultRetryBackoff = 2.0
	VMReadyTimeout      = 15 * time.Minute
	VMCreationTimeout   = 10 * time.Minute
)

// RetryConfig holds retry configuration
type RetryConfig struct {
	MaxRetries int
	Delay      time.Duration
	Backoff    float64
}

// getOrCreateTempKubeconfig creates a temporary kubeconfig file with the specified context set as current-context
// This is necessary because virtctl ssh does NOT respect the --context flag and always uses current-context
// The temp file is cached and reused for subsequent calls
func (c *Client) getOrCreateTempKubeconfig() (string, error) {
	// Return cached path if already created
	if c.tempKubeconfigPath != "" {
		return c.tempKubeconfigPath, nil
	}

	originalPath := os.Getenv("KUBECONFIG")
	if originalPath == "" || c.kubernetesContext == "" {
		return originalPath, nil // No need for temp file if no context switching needed
	}

	// Read the original kubeconfig
	data, err := os.ReadFile(originalPath)
	if err != nil {
		return "", fmt.Errorf("failed to read kubeconfig: %w", err)
	}

	// Create a temporary file
	tmpFile, err := os.CreateTemp("/tmp", "kubeconfig-*.yaml")
	if err != nil {
		return "", fmt.Errorf("failed to create temp kubeconfig: %w", err)
	}
	defer tmpFile.Close()

	tmpPath := tmpFile.Name()

	// Write the content and modify current-context using kubectl
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write temp kubeconfig: %w", err)
	}

	// Use kubectl to set the current-context
	cmd := exec.Command("kubectl", "config", "use-context", c.kubernetesContext, "--kubeconfig="+tmpPath)
	if err := cmd.Run(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to set context in temp kubeconfig: %w", err)
	}

	c.logger.WithFields(logrus.Fields{
		"tempKubeconfig": tmpPath,
		"context":        c.kubernetesContext,
	}).Debug("Created temporary kubeconfig with context set")

	// Cache the path for future calls
	c.tempKubeconfigPath = tmpPath

	return tmpPath, nil
}

// buildVirtctlSSHArgs builds standardized virtctl ssh arguments
// Note: virtctl ssh does NOT respect --context flag, so we use a temp kubeconfig with correct current-context
func (c *Client) buildVirtctlSSHArgs(namespace, vmName, username string, command string) []string {
	args := []string{}

	// Create temp kubeconfig with correct current-context (virtctl doesn't respect --context flag)
	if kubeconfigPath := os.Getenv("KUBECONFIG"); kubeconfigPath != "" {
		if tempPath, err := c.getOrCreateTempKubeconfig(); err == nil && tempPath != "" {
			args = append(args, "--kubeconfig="+tempPath)
		} else {
			// Fallback to original path if temp creation fails
			c.logger.WithError(err).Warn("Failed to create temp kubeconfig, using original")
			args = append(args, "--kubeconfig="+kubeconfigPath)
			if c.kubernetesContext != "" {
				args = append(args, "--context="+c.kubernetesContext)
			}
		}
	}

	// Then add the ssh subcommand and its arguments
	args = append(args,
		"ssh",
		fmt.Sprintf("vmi/%s", vmName),
		"--namespace="+namespace,
		"--username="+username,
		"--local-ssh-opts=-o StrictHostKeyChecking=no",
		"--local-ssh-opts=-o UserKnownHostsFile=/dev/null",
		"--local-ssh-opts=-o LogLevel=ERROR",
		"--local-ssh-opts=-i /home/appuser/.ssh/id_ed25519",
	)

	if command != "" {
		args = append(args, "--command="+command)
	}

	return args
}

// getDefaultRetryConfig returns default retry configuration
func getDefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: DefaultMaxRetries,
		Delay:      DefaultRetryDelay,
		Backoff:    DefaultRetryBackoff,
	}
}

// retryOperation executes an operation with exponential backoff retry
func (c *Client) retryOperation(ctx context.Context, operationName string, operation func() error) error {
	config := getDefaultRetryConfig()
	var lastErr error

	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(float64(config.Delay) * math.Pow(config.Backoff, float64(attempt-1)))
			c.logger.WithFields(logrus.Fields{
				"operation": operationName,
				"attempt":   attempt,
				"delay":     delay,
			}).Warn("Retrying operation after failure")

			select {
			case <-ctx.Done():
				return fmt.Errorf("operation cancelled: %w", ctx.Err())
			case <-time.After(delay):
				// Continue with retry
			}
		}

		err := operation()
		if err == nil {
			if attempt > 0 {
				c.logger.WithFields(logrus.Fields{
					"operation": operationName,
					"attempt":   attempt,
				}).Info("Operation succeeded after retry")
			}
			return nil
		}

		lastErr = err
		c.logger.WithError(err).WithFields(logrus.Fields{
			"operation": operationName,
			"attempt":   attempt,
		}).Warn("Operation failed")

		// Don't retry on context cancellation
		if ctx.Err() != nil {
			return fmt.Errorf("operation cancelled: %w", ctx.Err())
		}
	}

	return fmt.Errorf("operation %s failed after %d attempts: %w", operationName, config.MaxRetries+1, lastErr)
}

// NewClient creates a new KubeVirt client
func NewClient(restConfig *rest.Config, logger *logrus.Logger) (*Client, error) {
	// Create kubernetes client
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	// Create kubevirt client with the same config (already has correct context)
	virtClient, err := kubecli.GetKubevirtClientFromRESTConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubevirt client: %v", err)
	}

	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %v", err)
	}

	// Create and cache templates
	templateCache, err := loadTemplates(cfg.TemplatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load templates: %v", err)
	}

	// Test the KubeVirt client connection
	_, err = virtClient.VirtualMachine("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to KubeVirt API: %v", err)
	}

	return &Client{
		kubeClient:        kubeClient,
		virtClient:        virtClient,
		config:            cfg,
		restConfig:        restConfig,
		templateCache:     templateCache,
		logger:            logger,
		kubernetesContext: cfg.KubernetesContext, // STORE THE CONTEXT
	}, nil
}

// validateGoldenImage checks if the golden image PVC exists
func (c *Client) validateGoldenImage(ctx context.Context) error {
	if !c.config.ValidateGoldenImage {
		return nil // Skip validation if disabled
	}

	c.logger.WithFields(logrus.Fields{
		"imageName":      c.config.GoldenImageName,
		"imageNamespace": c.config.GoldenImageNamespace,
	}).Info("Validating golden image exists")

	// Check if the PVC exists
	_, err := c.kubeClient.CoreV1().PersistentVolumeClaims(c.config.GoldenImageNamespace).Get(
		ctx,
		c.config.GoldenImageName,
		metav1.GetOptions{},
	)

	if err != nil {
		return fmt.Errorf("golden image PVC '%s' not found in namespace '%s': %w",
			c.config.GoldenImageName,
			c.config.GoldenImageNamespace,
			err)
	}

	c.logger.WithField("imageName", c.config.GoldenImageName).Info("Golden image validation successful")
	return nil
}

func (c *Client) CreateCluster(ctx context.Context, namespace, controlPlaneName, workerNodeName string) error {
	// Validate golden image exists before proceeding
	err := c.validateGoldenImage(ctx)
	if err != nil {
		return fmt.Errorf("golden image validation failed: %w", err)
	}

	c.logger.WithFields(logrus.Fields{
		"namespace":    namespace,
		"controlPlane": controlPlaneName,
		"workerNode":   workerNodeName,
	}).Info("Starting VM cluster creation with enhanced error handling")

	// Step 1: Create control plane cloud-init secret with retry
	err = c.retryOperation(ctx, "create-control-plane-secret", func() error {
		return c.createCloudInitSecret(ctx, namespace, controlPlaneName, "control-plane")
	})
	if err != nil {
		return fmt.Errorf("failed to create control plane cloud-init secret: %w", err)
	}
	c.logger.Info("Control plane cloud-init secret created successfully")

	// Step 2: Create control plane VM with retry
	err = c.retryOperation(ctx, "create-control-plane-vm", func() error {
		return c.createVM(ctx, namespace, controlPlaneName, "control-plane")
	})
	if err != nil {
		return fmt.Errorf("failed to create control plane VM: %w", err)
	}
	c.logger.Info("Control plane VM created successfully")

	// Step 3: Wait for control plane to be ready with timeout
	controlPlaneCtx, cancelCP := context.WithTimeout(ctx, VMReadyTimeout)
	defer cancelCP()

	err = c.WaitForVMReady(controlPlaneCtx, namespace, controlPlaneName)
	if err != nil {
		// Try to cleanup on failure
		cleanupErr := c.cleanupFailedVM(ctx, namespace, controlPlaneName)
		if cleanupErr != nil {
			c.logger.WithError(cleanupErr).Error("Failed to cleanup control plane VM after creation failure")
		}
		return fmt.Errorf("control plane VM failed to become ready: %w", err)
	}
	c.logger.Info("Control plane VM is ready")

	// Step 4: Get join command with retry
	var joinCommand string
	err = c.retryOperation(ctx, "get-join-command", func() error {
		var cmdErr error
		joinCommand, cmdErr = c.getJoinCommand(ctx, namespace, controlPlaneName)
		return cmdErr
	})
	if err != nil {
		return fmt.Errorf("failed to get join command: %w", err)
	}

	// Step 5: Create worker node cloud-init secret with join command
	err = c.retryOperation(ctx, "create-worker-secret", func() error {
		return c.createCloudInitSecret(ctx, namespace, workerNodeName, "worker", map[string]string{
			"JOIN_COMMAND":           joinCommand,
			"JOIN":                   joinCommand,
			"CONTROL_PLANE_ENDPOINT": fmt.Sprintf("%s.%s.pod.cluster.local", strings.ReplaceAll(c.GetVMIP(ctx, namespace, controlPlaneName), ".", "-"), namespace),
			"CONTROL_PLANE_IP":       c.GetVMIP(ctx, namespace, controlPlaneName),
			"CONTROL_PLANE_VM_NAME":  controlPlaneName,
		})
	})
	if err != nil {
		return fmt.Errorf("failed to create worker node cloud-init secret: %w", err)
	}

	// Step 6: Create worker node VM with retry
	err = c.retryOperation(ctx, "create-worker-vm", func() error {
		return c.createVM(ctx, namespace, workerNodeName, "worker")
	})
	if err != nil {
		return fmt.Errorf("failed to create worker node VM: %w", err)
	}

	c.logger.WithFields(logrus.Fields{
		"namespace":    namespace,
		"controlPlane": controlPlaneName,
		"workerNode":   workerNodeName,
	}).Info("VM cluster creation completed successfully")

	return nil
}

// cleanupFailedVM cleans up a failed VM and its resources
func (c *Client) cleanupFailedVM(ctx context.Context, namespace, vmName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace": namespace,
		"vmName":    vmName,
	}).Info("Cleaning up failed VM")

	var errors []error

	// Delete VM (ignore not found errors)
	err := c.virtClient.VirtualMachine(namespace).Delete(ctx, vmName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		errors = append(errors, fmt.Errorf("failed to delete VM %s: %w", vmName, err))
	}

	// Delete DataVolume
	dvName := fmt.Sprintf("%s-rootdisk", vmName)
	err = c.virtClient.CdiClient().CdiV1beta1().DataVolumes(namespace).Delete(ctx, dvName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		errors = append(errors, fmt.Errorf("failed to delete DataVolume %s: %w", dvName, err))
	}

	// Delete cloud-init secret
	err = c.kubeClient.CoreV1().Secrets(namespace).Delete(ctx, vmName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		errors = append(errors, fmt.Errorf("failed to delete Secret %s: %w", vmName, err))
	}

	if len(errors) > 0 {
		var errorMsgs []string
		for _, e := range errors {
			errorMsgs = append(errorMsgs, e.Error())
		}
		return fmt.Errorf("cleanup errors: %s", strings.Join(errorMsgs, "; "))
	}

	c.logger.WithField("vmName", vmName).Info("VM cleanup completed successfully")
	return nil
}

func (c *Client) createCloudInitSecret(ctx context.Context, namespace, vmName, vmType string, extraVars ...map[string]string) error {
	// Load cloud-init template
	var templateName string
	if vmType == "control-plane" {
		templateName = "control-plane-cloud-config.yaml"
	} else {
		templateName = "worker-node-cloud-config.yaml"
	}

	// Create data map for template
	data := map[string]string{
		"CONTROL_PLANE_VM_NAME": fmt.Sprintf("cp-%s", namespace),
		"WORKER_VM_NAME":        fmt.Sprintf("wk-%s", namespace),
		"SESSION_NAMESPACE":     namespace,
		"SESSION_ID":            strings.TrimPrefix(namespace, "cluster"),
		"K8S_VERSION":           c.config.KubernetesVersion,
		"POD_CIDR":              c.config.PodCIDR,
	}

	// Add extra variables if provided
	if len(extraVars) > 0 {
		for k, v := range extraVars[0] {
			data[k] = v
		}
	}

	// Read template file
	templateContent, err := os.ReadFile(filepath.Join(c.config.TemplatePath, templateName))
	if err != nil {
		return fmt.Errorf("failed to read template file: %w", err)
	}

	// Substitute environment variables
	renderedConfig := substituteEnvVars(string(templateContent), data)

	// Properly encode cloud-init data in base64
	encodedConfig := base64Encode(renderedConfig)

	// Create secret
	var secretTemplate string
	if vmType == "control-plane" {
		secretTemplate = "control-plane-cloud-config-secret.yaml"
	} else {
		secretTemplate = "worker-node-cloud-config-secret.yaml"
	}

	// Set userdata in template data
	if vmType == "control-plane" {
		data["CONTROL_PLANE_USERDATA"] = encodedConfig
	} else {
		data["WORKER_USERDATA"] = encodedConfig
	}

	// Read the secret template file
	secretContent, err := os.ReadFile(filepath.Join(c.config.TemplatePath, secretTemplate))
	if err != nil {
		return fmt.Errorf("failed to read secret template file: %w", err)
	}

	// Substitute variables in the secret template
	renderedSecret := substituteEnvVars(string(secretContent), data)

	// Apply secret using kubectl
	return c.applyYAML(ctx, renderedSecret)
}

func (c *Client) createVM(ctx context.Context, namespace, vmName, vmType string) error {
	// Load VM template
	var templateName string
	if vmType == "control-plane" {
		templateName = "control-plane-template.yaml"
	} else {
		templateName = "worker-node-template.yaml"
	}

	// Create data map for template
	data := map[string]string{
		"CONTROL_PLANE_VM_NAME":  fmt.Sprintf("cp-%s", namespace),
		"WORKER_VM_NAME":         fmt.Sprintf("wk-%s", namespace),
		"SESSION_NAMESPACE":      namespace,
		"SESSION_ID":             namespace,
		"K8S_VERSION":            c.config.KubernetesVersion,
		"CPU_CORES":              c.config.VMCPUCores,
		"MEMORY":                 c.config.VMMemory,
		"STORAGE_SIZE":           c.config.VMStorageSize,
		"STORAGE_CLASS":          c.config.VMStorageClass,
		"IMAGE_URL":              c.config.VMImageURL,
		"POD_CIDR":               c.config.PodCIDR,
		"GOLDEN_IMAGE_NAME":      c.config.GoldenImageName,
		"GOLDEN_IMAGE_NAMESPACE": c.config.GoldenImageNamespace,
	}

	// Read the VM template file
	templateContent, err := os.ReadFile(filepath.Join(c.config.TemplatePath, templateName))
	if err != nil {
		return fmt.Errorf("failed to read VM template file: %w", err)
	}

	// Substitute variables in the VM template
	renderedVM := substituteEnvVars(string(templateContent), data)
	c.logger.WithFields(logrus.Fields{
		"vmName":    vmName,
		"vmType":    vmType,
		"namespace": namespace,
	}).Debug("Rendered VM YAML content:")
	c.logger.Debug("=== RENDERED VM YAML START ===")
	c.logger.Debug(renderedVM)
	c.logger.Debug("=== RENDERED VM YAML END ===")
	// Apply VM using kubectl
	return c.applyYAML(ctx, renderedVM)
}

// WaitForVMsReady waits for multiple VMs to be ready
func (c *Client) WaitForVMsReady(ctx context.Context, namespace string, vmNames ...string) error {
	for _, vmName := range vmNames {
		if err := c.WaitForVMReady(ctx, namespace, vmName); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) WaitForVMReady(ctx context.Context, namespace, vmName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace": namespace,
		"vmName":    vmName,
	}).Info("Waiting for VM to become ready")

	startTime := time.Now()
	return wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(context.Context) (bool, error) {
		// Check VM exists and get status
		vm, err := c.virtClient.VirtualMachine(namespace).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				elapsed := time.Since(startTime)
				c.logger.WithFields(logrus.Fields{
					"vmName":  vmName,
					"elapsed": elapsed,
				}).Debug("VM not found yet, continuing to wait...")
				return false, nil
			}
			// Log error but continue trying
			c.logger.WithError(err).WithField("vmName", vmName).Warn("Error checking VM status, retrying...")
			return false, nil
		}

		// Log detailed VM status for debugging
		c.logger.WithFields(logrus.Fields{
			"vmName":  vmName,
			"running": vm.Spec.Running,
			"created": vm.Status.Created,
			"ready":   vm.Status.Ready,
			"elapsed": time.Since(startTime),
		}).Debug("VM status check")

		// Check VMI status for more detailed information
		vmi, err := c.virtClient.VirtualMachineInstance(namespace).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				c.logger.WithField("vmName", vmName).Debug("VMI not found yet, VM not fully created")
				return false, nil
			}
			c.logger.WithError(err).WithField("vmName", vmName).Warn("Error checking VMI status")
			return false, nil
		}

		// Log VMI phase for debugging
		c.logger.WithFields(logrus.Fields{
			"vmName":  vmName,
			"phase":   vmi.Status.Phase,
			"elapsed": time.Since(startTime),
		}).Debug("VMI status check")

		// Check if VMI is in Running phase AND VM is marked as ready
		if vmi.Status.Phase == "Running" && vm.Status.Ready {
			elapsed := time.Since(startTime)
			c.logger.WithFields(logrus.Fields{
				"vmName":  vmName,
				"elapsed": elapsed,
			}).Info("VM is ready and running")
			return true, nil
		}

		// Check if VMI is in Running phase for extended period (fallback)
		if vmi.Status.Phase == "Running" {
			if vmi.Status.PhaseTransitionTimestamps != nil {
				for _, transition := range vmi.Status.PhaseTransitionTimestamps {
					if transition.Phase == "Running" {
						runningDuration := time.Since(transition.PhaseTransitionTimestamp.Time)
						if runningDuration > 60*time.Second {
							c.logger.WithFields(logrus.Fields{
								"vmName":     vmName,
								"runningFor": runningDuration,
							}).Info("VM has been running for 60+ seconds, considering it ready")
							return true, nil
						}
					}
				}
			}
		}

		// Check for failed states
		if vmi.Status.Phase == "Failed" {
			return false, fmt.Errorf("VM %s failed to start: phase is Failed", vmName)
		}

		// Continue waiting
		elapsed := time.Since(startTime)
		c.logger.WithFields(logrus.Fields{
			"vmName":   vmName,
			"vmiPhase": vmi.Status.Phase,
			"vmReady":  vm.Status.Ready,
			"elapsed":  elapsed,
		}).Debug("VM not ready yet, continuing to wait...")
		return false, nil
	})
}

func (c *Client) VerifyKubeVirtAvailable(ctx context.Context) error {
	c.logger.Info("Verifying KubeVirt availability")

	// Try to list VMs in the default namespace as a check
	_, err := c.virtClient.VirtualMachine("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.WithError(err).Error("Failed to access KubeVirt API")
		return fmt.Errorf("failed to access KubeVirt API: %w", err)
	}

	c.logger.Info("KubeVirt API is accessible")
	return nil
}

func (c *Client) getJoinCommand(ctx context.Context, namespace, controlPlaneName string) (string, error) {
	c.logger.WithFields(logrus.Fields{
		"namespace":        namespace,
		"controlPlaneName": controlPlaneName,
	}).Info("Getting join command from control plane")

	// Adjust the VM name to match the actual name pattern
	actualVMName := fmt.Sprintf("cp-%s", namespace)
	c.logger.WithField("actualVMName", actualVMName).Info("Adjusted VM name for join command")

	// Wait for the VM to be fully ready with kubelet initialized
	time.Sleep(60 * time.Second)

	// Simple direct attempt without polling first
	c.logger.Info("Attempting direct join command retrieval...")

	// Use buildVirtctlSSHArgs to ensure kubeconfig and context are included
	args := c.buildVirtctlSSHArgs(namespace, actualVMName, "suporte", "cat /etc/kubeadm-join-command")
	cmd := exec.Command("virtctl", args...)

	// IMPORTANT: virtctl ignores --kubeconfig flag, so we override KUBECONFIG env var
	// We set KUBECONFIG to our temp kubeconfig path instead of unsetting it completely
	var envWithCustomKubeconfig []string
	tempKubeconfigPath, _ := c.getOrCreateTempKubeconfig()
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "KUBECONFIG=") {
			envWithCustomKubeconfig = append(envWithCustomKubeconfig, env)
		}
	}
	if tempKubeconfigPath != "" {
		envWithCustomKubeconfig = append(envWithCustomKubeconfig, "KUBECONFIG="+tempKubeconfigPath)
	}
	cmd.Env = envWithCustomKubeconfig

	c.logger.WithField("command", cmd.String()).Debug("Executing virtctl command")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		c.logger.WithError(err).WithField("stderr", stderr.String()).Error("Direct join command attempt failed")
		return "", fmt.Errorf("failed to execute join command: %v", err)
	}

	output := stdout.String()
	joinCommand := strings.TrimSpace(output)

	if joinCommand == "" {
		c.logger.Error("Join command is empty")
		return "", fmt.Errorf("join command is empty")
	}

	c.logger.WithField("joinCommand", joinCommand).Info("Successfully retrieved join command")
	return joinCommand, nil
}

// GetVMIP gets the IP address of a VM
func (c *Client) GetVMIP(ctx context.Context, namespace, vmName string) string {
	var ip string
	err := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		// Get VM instance
		vmi, err := c.virtClient.VirtualMachineInstance(namespace).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			return false, nil // Keep trying
		}

		// Check if any interfaces exist
		if len(vmi.Status.Interfaces) == 0 {
			return false, nil
		}

		// Get IP from first interface
		ip = vmi.Status.Interfaces[0].IP
		if ip != "" {
			return true, nil
		}

		return false, nil
	})

	if err != nil {
		// Return placeholder if IP retrieval failed
		return "0.0.0.0"
	}

	return ip
}

// DeleteVMs deletes VMs and associated resources
func (c *Client) DeleteVMs(ctx context.Context, namespace string, vmNames ...string) error {
	for _, vmName := range vmNames {
		// Delete VM
		err := c.virtClient.VirtualMachine(namespace).Delete(ctx, vmName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete VM %s: %v", vmName, err)
		}

		// Delete DataVolume
		dvName := fmt.Sprintf("%s-rootdisk", vmName)
		err = c.virtClient.CdiClient().CdiV1beta1().DataVolumes(namespace).Delete(ctx, dvName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete DataVolume %s: %v", dvName, err)
		}

		// Delete cloud-init secret
		err = c.kubeClient.CoreV1().Secrets(namespace).Delete(ctx, vmName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete Secret %s: %v", vmName, err)
		}
	}

	return nil
}

// Update the method signature to include retry parameter
func (c *Client) ExecuteCommandInVM(ctx context.Context, namespace, vmName, command string, retry ...bool) (string, error) {
	// Default retry to true for backward compatibility
	shouldRetry := true
	if len(retry) > 0 {
		shouldRetry = retry[0]
	}

	c.logger.WithFields(logrus.Fields{
		"vmName":    vmName,
		"namespace": namespace,
		"command":   command,
		"retry":     shouldRetry,
	}).Debug("Executing command in VM")

	// Rest of the existing logic, but wrap the actual execution
	if shouldRetry {
		// Use existing retry logic
		var output string
		err := c.retryOperation(ctx, fmt.Sprintf("ssh-execute-%s", vmName), func() error {
			var cmdErr error
			output, cmdErr = c.executeCommandDirect(ctx, namespace, vmName, command)
			return cmdErr
		})
		return output, err
	} else {
		// Execute directly without retry
		return c.executeCommandDirect(ctx, namespace, vmName, command)
	}
}

// Extract the actual command execution logic into a separate method
func (c *Client) executeCommandDirect(ctx context.Context, namespace, vmName, command string) (string, error) {
	// Move the existing command execution logic here
	args := c.buildVirtctlSSHArgs(namespace, vmName, "suporte", command)

	cmd := exec.CommandContext(ctx, "virtctl", args...)

	// IMPORTANT: virtctl ignores --kubeconfig flag, so we override KUBECONFIG env var
	var envWithCustomKubeconfig []string
	tempKubeconfigPath, _ := c.getOrCreateTempKubeconfig()
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "KUBECONFIG=") {
			envWithCustomKubeconfig = append(envWithCustomKubeconfig, env)
		}
	}
	if tempKubeconfigPath != "" {
		envWithCustomKubeconfig = append(envWithCustomKubeconfig, "KUBECONFIG="+tempKubeconfigPath)
	}
	cmd.Env = envWithCustomKubeconfig

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil{
		stderrStr := stderr.String()
		return "", fmt.Errorf("SSH command execution failed on VM %s: %w, stderr: %s", vmName, err, stderrStr)
	}

	return stdout.String(), nil
}

// substituteEnvVars replaces ${VAR} with the value of the environment variable VAR
func substituteEnvVars(input string, vars map[string]string) string {
	result := input

	// Regular expression to find ${VAR} patterns
	re := regexp.MustCompile(`\${([A-Za-z0-9_]+)}`)

	// Replace all occurrences
	result = re.ReplaceAllStringFunc(result, func(match string) string {
		// Extract variable name without ${ and }
		varName := match[2 : len(match)-1]

		// Look up the value in vars map first, then in environment
		if value, ok := vars[varName]; ok {
			return value
		}

		// If not in vars map, try environment
		if value, ok := os.LookupEnv(varName); ok {
			return value
		}

		// If not found, return the original ${VAR}
		return match
	})

	return result
}

// loadTemplates loads all template files from a directory
func loadTemplates(templatePath string) (map[string]*template.Template, error) {
	templates := make(map[string]*template.Template)

	// Template files to load
	templateFiles := []string{
		"control-plane-cloud-config.yaml",
		"worker-node-cloud-config.yaml",
		"control-plane-cloud-config-secret.yaml",
		"worker-node-cloud-config-secret.yaml",
		"control-plane-template.yaml",
		"worker-node-template.yaml",
	}

	for _, fileName := range templateFiles {
		filePath := filepath.Join(templatePath, fileName)

		// Read template file
		tmplContent, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read template file %s: %v", filePath, err)
		}

		// Parse template
		tmpl, err := template.New(fileName).Parse(string(tmplContent))
		if err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %v", fileName, err)
		}

		templates[fileName] = tmpl
	}

	return templates, nil
}

// base64Encode encodes a string to base64
func base64Encode(input string) string {
	return base64.StdEncoding.EncodeToString([]byte(input))
}

// applyYAML applies YAML to the cluster
func (c *Client) applyYAML(ctx context.Context, yaml string) error {
	args := []string{"apply", "-f", "-"}
	if c.kubernetesContext != "" {
		args = append([]string{"--context", c.kubernetesContext}, args...)
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)

	tempKubeconfigPath, _ := c.getOrCreateTempKubeconfig()
	if tempKubeconfigPath != "" {
		var env []string
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "KUBECONFIG=") {
				env = append(env, e)
			}
		}
		env = append(env, "KUBECONFIG="+tempKubeconfigPath)
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start kubectl apply: %w", err)
	}

	io.WriteString(stdin, yaml)
	stdin.Close()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("kubectl apply failed: %w, stderr: %s", err, stderr.String())
	}

	return nil
}

// GetVMStatus gets the status of a VM
func (c *Client) GetVMStatus(ctx context.Context, namespace, vmName string) (string, error) {
	vm, err := c.virtClient.VirtualMachine(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	if vm.Status.Ready {
		return "Running", nil
	}

	if vm.Status.Created {
		return "Starting", nil
	}

	return "Pending", nil
}

// CreateVMSnapshot creates a snapshot of a virtual machine
func (c *Client) CreateVMSnapshot(ctx context.Context, namespace, vmName, snapshotName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace":    namespace,
		"vmName":       vmName,
		"snapshotName": snapshotName,
	}).Info("Creating VM snapshot")

	snapshot := &snapshotv1beta1.VirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotName,
			Namespace: namespace,
			Labels: map[string]string{
				"cks.io/snapshot": "base-cluster",
				"cks.io/vm-role": func() string {
					if strings.Contains(vmName, "control-plane") {
						return "control-plane"
					}
					return "worker"
				}(),
			},
		},
		Spec: snapshotv1beta1.VirtualMachineSnapshotSpec{
			Source: corev1.TypedLocalObjectReference{
				APIGroup: &[]string{"kubevirt.io"}[0], // Add the API group
				Kind:     "VirtualMachine",
				Name:     vmName,
			},
		},
	}

	_, err := c.virtClient.VirtualMachineSnapshot(namespace).Create(ctx, snapshot, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create snapshot %s: %w", snapshotName, err)
	}

	c.logger.WithField("snapshotName", snapshotName).Info("VM snapshot creation initiated")
	return nil
}

// WaitForSnapshotReady waits for a snapshot to be ready to use
func (c *Client) WaitForSnapshotReady(ctx context.Context, namespace, snapshotName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace":    namespace,
		"snapshotName": snapshotName,
	}).Info("Waiting for snapshot to be ready")

	startTime := time.Now()
	return wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(context.Context) (bool, error) {
		snapshot, err := c.virtClient.VirtualMachineSnapshot(namespace).Get(ctx, snapshotName, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				c.logger.WithField("snapshotName", snapshotName).Debug("Snapshot not found yet")
				return false, nil
			}
			c.logger.WithError(err).WithField("snapshotName", snapshotName).Warn("Error checking snapshot status")
			return false, nil
		}

		elapsed := time.Since(startTime)
		c.logger.WithFields(logrus.Fields{
			"snapshotName": snapshotName,
			"elapsed":      elapsed,
			"phase": func() string {
				if snapshot.Status != nil {
					return string(snapshot.Status.Phase)
				}
				return "Unknown"
			}(),
			"readyToUse": func() bool {
				if snapshot.Status != nil && snapshot.Status.ReadyToUse != nil {
					return *snapshot.Status.ReadyToUse
				}
				return false
			}(),
		}).Debug("Snapshot status check")

		if snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse {
			c.logger.WithFields(logrus.Fields{
				"snapshotName": snapshotName,
				"elapsed":      elapsed,
			}).Info("Snapshot is ready")
			return true, nil
		}

		// Check for failed state
		if snapshot.Status != nil && snapshot.Status.Phase == snapshotv1beta1.Failed {
			return false, fmt.Errorf("snapshot %s failed to create", snapshotName)
		}

		return false, nil
	})
}

// CheckSnapshotExists checks if a snapshot exists and is ready
func (c *Client) CheckSnapshotExists(ctx context.Context, namespace, snapshotName string) bool {
	snapshot, err := c.virtClient.VirtualMachineSnapshot(namespace).Get(ctx, snapshotName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	return snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse
}

// DeleteVMSnapshot deletes a VM snapshot
func (c *Client) DeleteVMSnapshot(ctx context.Context, namespace, snapshotName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace":    namespace,
		"snapshotName": snapshotName,
	}).Info("Deleting VM snapshot")

	err := c.virtClient.VirtualMachineSnapshot(namespace).Delete(ctx, snapshotName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete snapshot %s: %w", snapshotName, err)
	}

	c.logger.WithField("snapshotName", snapshotName).Info("VM snapshot deleted")
	return nil
}

// StartVM starts a virtual machine
func (c *Client) StartVM(ctx context.Context, namespace, vmName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace": namespace,
		"vmName":    vmName,
	}).Info("Starting VM")

	vm, err := c.virtClient.VirtualMachine(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get VM %s: %w", vmName, err)
	}

	// Set running to true
	vm.Spec.Running = &[]bool{true}[0]
	_, err = c.virtClient.VirtualMachine(namespace).Update(ctx, vm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to start VM %s: %w", vmName, err)
	}

	c.logger.WithField("vmName", vmName).Info("VM start initiated")
	return nil
}

// VirtClient returns the KubeVirt client for direct API access
func (c *Client) VirtClient() kubecli.KubevirtClient {
	return c.virtClient
}

// StopVMs stops multiple VMs for consistent snapshot creation
func (c *Client) StopVMs(ctx context.Context, namespace string, vmNames ...string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace": namespace,
		"vmNames":   vmNames,
	}).Info("Freezing VMs for snapshot")

	errChan := make(chan error, len(vmNames))

	// Stop all VMs in parallel
	for _, vmName := range vmNames {
		go func(name string) {
			vm, err := c.virtClient.VirtualMachine(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				errChan <- fmt.Errorf("failed to get VM %s: %w", name, err)
				return
			}

			// Set running to false
			vm.Spec.Running = &[]bool{false}[0]
			_, err = c.virtClient.VirtualMachine(namespace).Update(ctx, vm, metav1.UpdateOptions{})
			if err != nil {
				errChan <- fmt.Errorf("failed to stop VM %s: %w", name, err)
				return
			}

			// Wait for VM to stop
			err = c.waitForVMStopped(ctx, namespace, name)
			errChan <- err
		}(vmName)
	}

	// Wait for all VMs to stop
	for range vmNames {
		if err := <-errChan; err != nil {
			return err
		}
	}

	c.logger.WithField("vmNames", vmNames).Info("All VMs stopped successfully")
	return nil
}

// waitForVMStopped waits for a VM to be completely stopped
func (c *Client) waitForVMStopped(ctx context.Context, namespace, vmName string) error {
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(context.Context) (bool, error) {
		// Check if VMI still exists
		_, err := c.virtClient.VirtualMachineInstance(namespace).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				// VMI doesn't exist anymore, VM is stopped
				return true, nil
			}
			return false, nil
		}
		// VMI still exists, VM is not fully stopped
		return false, nil
	})
}

// RestoreVMFromSnapshot restores a VM from its snapshot
func (c *Client) RestoreVMFromSnapshot(ctx context.Context, namespace, vmName, snapshotName string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace":    namespace,
		"vmName":       vmName,
		"snapshotName": snapshotName,
	}).Info("Starting VM restore from snapshot")

	// Step 1: Stop the VM
	err := c.StopVMs(ctx, namespace, vmName)
	if err != nil {
		return fmt.Errorf("failed to stop VM %s: %w", vmName, err)
	}

	// Wait a bit for cleanup to complete
	time.Sleep(10 * time.Second)

	// Step 4: Create VirtualMachineRestore from snapshot
	restore := &snapshotv1beta1.VirtualMachineRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-restore-%d", vmName, time.Now().Unix()),
			Namespace: namespace,
		},
		Spec: snapshotv1beta1.VirtualMachineRestoreSpec{
			Target: corev1.TypedLocalObjectReference{
				APIGroup: &[]string{"kubevirt.io"}[0],
				Kind:     "VirtualMachine",
				Name:     vmName,
			},
			VirtualMachineSnapshotName: snapshotName,
		},
	}

	_, err = c.virtClient.VirtualMachineRestore(namespace).Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create VM restore: %w", err)
	}

	// Step 5: Wait for restore to complete
	err = c.waitForRestoreComplete(ctx, namespace, restore.Name)
	if err != nil {
		return fmt.Errorf("restore failed to complete: %w", err)
	}

	// Step 6: Start the restored VM
	err = c.StartVM(ctx, namespace, vmName)
	if err != nil {
		return fmt.Errorf("failed to start restored VM: %w", err)
	}

	// Step 7: Wait for VM to be ready
	err = c.WaitForVMReady(ctx, namespace, vmName)
	if err != nil {
		return fmt.Errorf("restored VM failed to become ready: %w", err)
	}

	c.logger.WithFields(logrus.Fields{
		"vmName":       vmName,
		"snapshotName": snapshotName,
	}).Info("VM restore completed successfully")

	return nil
}

// waitForRestoreComplete waits for a VirtualMachineRestore to complete
func (c *Client) waitForRestoreComplete(ctx context.Context, namespace, restoreName string) error {
	c.logger.WithField("restoreName", restoreName).Info("Waiting for restore to complete")

	return wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(context.Context) (bool, error) {
		restore, err := c.virtClient.VirtualMachineRestore(namespace).Get(ctx, restoreName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}

		if restore.Status != nil && restore.Status.Complete != nil && *restore.Status.Complete {
			c.logger.WithField("restoreName", restoreName).Info("Restore completed successfully")
			return true, nil
		}

		// Check for failure
		if restore.Status != nil && len(restore.Status.Conditions) > 0 {
			for _, condition := range restore.Status.Conditions {
				if condition.Type == "Failure" && condition.Status == corev1.ConditionTrue {
					return false, fmt.Errorf("restore failed: %s", condition.Message)
				}
			}
		}

		return false, nil
	})
}

// IsVMSSHReady checks if a VM is ready for SSH connections
func (c *Client) IsVMSSHReady(ctx context.Context, namespace, vmName string) (bool, error) {
	c.logger.WithFields(logrus.Fields{
		"namespace": namespace,
		"vmName":    vmName,
	}).Debug("Checking if VM is SSH ready")

	// First check if VM is running
	status, err := c.GetVMStatus(ctx, namespace, vmName)
	if err != nil {
		return false, fmt.Errorf("failed to get VM status: %w", err)
	}

	if status != "Running" {
		c.logger.WithFields(logrus.Fields{
			"vmName": vmName,
			"status": status,
		}).Debug("VM is not in Running state")
		return false, nil
	}

	// Try a simple SSH connection test
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	args := c.buildVirtctlSSHArgs(namespace, vmName, "suporte", "echo 'ssh-ready-test'")
	cmd := exec.CommandContext(testCtx, "virtctl", args...)

	// IMPORTANT: virtctl ignores --kubeconfig flag, so we override KUBECONFIG env var
	var envWithCustomKubeconfig []string
	tempKubeconfigPath, _ := c.getOrCreateTempKubeconfig()
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "KUBECONFIG=") {
			envWithCustomKubeconfig = append(envWithCustomKubeconfig, env)
		}
	}
	if tempKubeconfigPath != "" {
		envWithCustomKubeconfig = append(envWithCustomKubeconfig, "KUBECONFIG="+tempKubeconfigPath)
	}
	cmd.Env = envWithCustomKubeconfig

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		c.logger.WithError(err).WithFields(logrus.Fields{
			"vmName": vmName,
			"stderr": stderr.String(),
		}).Debug("SSH readiness test failed")
		return false, nil
	}

	output := strings.TrimSpace(stdout.String())
	isReady := strings.Contains(output, "ssh-ready-test")

	c.logger.WithFields(logrus.Fields{
		"vmName":  vmName,
		"isReady": isReady,
		"output":  output,
	}).Debug("SSH readiness test completed")

	return isReady, nil
}

// CleanupOldRestores removes old VirtualMachineRestore objects and their associated PVCs
func (c *Client) CleanupOldRestores(ctx context.Context, namespace string, vmNames ...string) error {
	c.logger.WithFields(logrus.Fields{
		"namespace": namespace,
		"vmNames":   vmNames,
	}).Info("Starting cleanup of old VM restore objects and PVCs")

	startTime := time.Now()
	var cleanupStats struct {
		restoresDeleted int
		pvcsDeleted     int
		errors          []string
	}

	// Step 1: Find VirtualMachineRestore objects and collect their PVC names
	c.logger.Debug("Listing VirtualMachineRestore objects for cleanup")
	restores, err := c.virtClient.VirtualMachineRestore(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.WithError(err).Warn("Failed to list VirtualMachineRestore objects, skipping restore cleanup")
		return nil
	}

	// Create a pattern to match our VM names
	vmNamePattern := strings.Join(vmNames, "|")
	restorePattern := regexp.MustCompile(fmt.Sprintf("(%s).*restore", vmNamePattern))

	// Collect PVC names before deleting restore objects
	var restorePVCsToDelete []string

	for _, restore := range restores.Items {
		// Check if this restore is related to one of our VMs
		isRelated := false

		// Check by target VM name
		if restore.Spec.Target.Name != "" {
			for _, vmName := range vmNames {
				if restore.Spec.Target.Name == vmName {
					isRelated = true
					break
				}
			}
		}

		// Also check by restore name pattern
		if !isRelated && restorePattern.MatchString(restore.Name) {
			isRelated = true
		}

		if isRelated {
			c.logger.WithFields(logrus.Fields{
				"restoreName": restore.Name,
				"targetVM":    restore.Spec.Target.Name,
				"age":         time.Since(restore.CreationTimestamp.Time),
				"complete":    restore.Status != nil && restore.Status.Complete != nil && *restore.Status.Complete,
			}).Info("Found VirtualMachineRestore for cleanup")

			// Extract PVC names from restore status before deleting
			if restore.Status != nil && restore.Status.Restores != nil {
				for _, restoreInfo := range restore.Status.Restores {
					if restoreInfo.PersistentVolumeClaimName != "" {
						restorePVCsToDelete = append(restorePVCsToDelete, restoreInfo.PersistentVolumeClaimName)
						c.logger.WithFields(logrus.Fields{
							"restoreName": restore.Name,
							"pvcName":     restoreInfo.PersistentVolumeClaimName,
							"volumeName":  restoreInfo.VolumeName,
						}).Debug("Collected PVC name for deletion")
					}
				}
			}

			// Delete the VirtualMachineRestore object
			err := c.virtClient.VirtualMachineRestore(namespace).Delete(ctx, restore.Name, metav1.DeleteOptions{})
			if err != nil && !k8serrors.IsNotFound(err) {
				errMsg := fmt.Sprintf("failed to delete restore %s: %v", restore.Name, err)
				c.logger.WithError(err).WithField("restoreName", restore.Name).Warn("Failed to delete VirtualMachineRestore")
				cleanupStats.errors = append(cleanupStats.errors, errMsg)
			} else {
				cleanupStats.restoresDeleted++
				c.logger.WithField("restoreName", restore.Name).Info("Deleted VirtualMachineRestore object")
			}
		}
	}

	// Step 2: Delete the collected restore PVCs
	c.logger.WithField("pvcCount", len(restorePVCsToDelete)).Info("Deleting restore PVCs")
	for _, pvcName := range restorePVCsToDelete {
		c.logger.WithField("pvcName", pvcName).Info("Deleting restore PVC")

		err := c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("failed to delete restore PVC %s: %v", pvcName, err)
			c.logger.WithError(err).WithField("pvcName", pvcName).Warn("Failed to delete restore PVC")
			cleanupStats.errors = append(cleanupStats.errors, errMsg)
		} else {
			cleanupStats.pvcsDeleted++
			c.logger.WithField("pvcName", pvcName).Info("Successfully deleted restore PVC")
		}
	}

	// Step 3: Also clean up any orphaned restore PVCs (fallback cleanup)
	c.logger.Debug("Scanning for orphaned restore PVCs")
	pvcs, err := c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.WithError(err).Warn("Failed to list PVCs for orphan cleanup")
	} else {
		// Look for PVCs that match restore patterns but weren't in our collected list
		restoreOrphanPattern := regexp.MustCompile(`^restore-[a-f0-9-]+-.*`)

		for _, pvc := range pvcs.Items {
			if restoreOrphanPattern.MatchString(pvc.Name) {
				// Check if we already processed this PVC
				alreadyProcessed := false
				for _, processedPVC := range restorePVCsToDelete {
					if processedPVC == pvc.Name {
						alreadyProcessed = true
						break
					}
				}

				if !alreadyProcessed {
					c.logger.WithFields(logrus.Fields{
						"pvcName": pvc.Name,
						"age":     time.Since(pvc.CreationTimestamp.Time),
					}).Info("Found orphaned restore PVC, deleting")

					err := c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
					if err != nil && !k8serrors.IsNotFound(err) {
						errMsg := fmt.Sprintf("failed to delete orphaned restore PVC %s: %v", pvc.Name, err)
						c.logger.WithError(err).WithField("pvcName", pvc.Name).Warn("Failed to delete orphaned restore PVC")
						cleanupStats.errors = append(cleanupStats.errors, errMsg)
					} else {
						cleanupStats.pvcsDeleted++
						c.logger.WithField("pvcName", pvc.Name).Info("Successfully deleted orphaned restore PVC")
					}
				}
			}
		}
	}

	// Log cleanup summary
	elapsed := time.Since(startTime)
	c.logger.WithFields(logrus.Fields{
		"namespace":       namespace,
		"restoresDeleted": cleanupStats.restoresDeleted,
		"pvcsDeleted":     cleanupStats.pvcsDeleted,
		"errors":          len(cleanupStats.errors),
		"duration":        elapsed,
	}).Info("Cleanup of old restore objects and PVCs completed")

	// Log errors if any
	if len(cleanupStats.errors) > 0 {
		c.logger.WithField("errors", cleanupStats.errors).Warn("Some cleanup operations failed")
	}

	return nil
}
