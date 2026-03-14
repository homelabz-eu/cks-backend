# CKS Backend

Go-based backend service for the Certified Kubernetes Security Specialist (CKS) training platform. Implements cluster pool management, snapshot-based VM restoration, and automated validation for security scenarios using KubeVirt.

## Architecture Overview

Production-grade Kubernetes-native learning platform that provisions isolated practice environments using KubeVirt virtual machines. The system implements a cluster pool pattern with snapshot-based restoration to provide sub-second session allocation while maintaining cost efficiency through resource sharing.

### Core Components

**Cluster Pool Manager** (`src/internal/clusterpool/`)
- Pre-provisions fixed pool of 3 Kubernetes clusters (control-plane + worker VMs)
- State persistence via Kubernetes namespace annotations (`cks.io/cluster-status`, `cks.io/last-reset`)
- Thread-safe cluster assignment using RWMutex for race-free operations
- Snapshot-based reset workflow achieving 5x faster restoration (2-3 minutes vs 10-15 minutes)
- Automatic cleanup of VirtualMachineRestore objects and associated PVCs to prevent storage exhaustion

**Session Manager** (`src/internal/sessions/`)
- Handles complete session lifecycle from cluster assignment to resource cleanup
- Sessions persisted to Redis via `SessionStore` interface, enabling horizontal scaling with multiple replicas
- Creates dedicated Kubernetes namespace per session with resource quotas (16 cores, 16Gi memory, 20 pods)
- Background expiration monitoring via goroutine running every 5 minutes
- Deterministic terminal ID generation (`{sessionID}-{target}`) enabling frontend reconnection
- Asynchronous scenario initialization allowing immediate user access to pre-bootstrapped clusters

**Scenario System** (`src/internal/scenarios/`)
- Filesystem-based scenario loading from structured directory tree
- YAML-defined scenarios with metadata, tasks, validation rules, and setup steps
- Markdown task descriptions parsed with objectives, hints, and step-by-step guides
- Template-based VM generation with variable substitution (20+ configurable parameters)
- Hot-reload capability via `/api/v1/scenarios/reload` endpoint

**Unified Validation Engine** (`src/internal/validation/`)
- Multi-strategy validator supporting resource existence, kubectl commands, script execution, and file content checks
- SSH integration via `virtctl ssh` for command execution and file validation
- Condition types: equals, contains, exists, not_exists, greater_than, less_than, matches_regex
- Consistent ValidationResponse format with detailed results per rule

**KubeVirt Integration** (`src/internal/kubevirt/`)
- VM creation from golden image PVC using DataVolume cloning
- Cloud-init templating with dynamic join command extraction for worker node provisioning
- Snapshot operations (create, wait for ready, restore) with comprehensive error handling
- Advanced cleanup strategy for VirtualMachineRestore artifacts preventing PVC accumulation
- Context management workaround for `virtctl ssh` (creates temp kubeconfig with correct current-context)

## DevOps Practices

### Immutable Infrastructure

**Golden Image Pattern**
- Base VM image (`ubuntu-2204-kube`) pre-installed with Kubernetes 1.33.0
- All clusters clone from identical golden PVC ensuring consistency
- Fast provisioning (PVC clone ~60s vs full kubeadm init ~10-15 minutes)
- Image stored in dedicated namespace (`vm-templates`) with lifecycle management

**Snapshot-Based Restoration**
- Baseline snapshots created after initial cluster bootstrap
- Reset workflow: Stop VMs → Cleanup old restores → Create VirtualMachineRestore → Wait for complete → Start VMs
- Immutable baseline state prevents configuration drift
- CoW (Copy-on-Write) snapshots minimize storage overhead

### Kubernetes-Native State Management

**Persistent State via Annotations**
```go
namespace.Annotations["cks.io/cluster-status"] = "available|locked|resetting|error"
namespace.Annotations["cks.io/last-reset"] = "2025-11-15T10:30:00Z"
namespace.Annotations["cks.io/created-at"] = "2025-11-15T08:00:00Z"
```

Benefits:
- Survives pod restarts without external database
- Atomic updates via Kubernetes API
- Distributed consensus via etcd
- Queryable via kubectl for debugging

**Cluster Pool: In-Memory Cache with Persistent Fallback**
- ClusterPoolManager maintains in-memory state for O(1) lookups
- On initialization, reads annotations to reconstruct state
- Writes to both in-memory and namespace annotations

**Session Storage: Redis**
- Sessions stored in Redis as JSON with TTL-based expiration
- Key pattern: `cks:session:{id}` for data, `cks:sessions` SET for index
- Enables multiple backend replicas with shared session state
- Health check endpoint validates Redis connectivity

### Multi-Stage Docker Build

**Build Stage** (golang:1.24-alpine)
```dockerfile
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags='-w -s -extldflags "-static"' \
  -a -installsuffix cgo \
  -o server ./cmd/server
```
- Static binary with no C dependencies
- Stripped debug symbols (`-w -s`) reducing size by ~30%
- Cross-compilation for Linux amd64

**Runtime Stage** (alpine:3.19)
- Non-root user execution (UID 1001)
- Minimal tooling: kubectl
- Health check endpoint configured
- Final image ~150MB vs ~800MB with full Go environment

### GitOps CI/CD Pipeline

**Automated Pipeline** (`.github/workflows/pipeline.yml`)

Job Flow:
1. **docker-build-and-push**: Multi-stage build, push to `registry.homelabz.eu/library/cks-backend` with commit SHA + latest tags
2. **dev-deploy**: Kustomize overlay deployment to development cluster
3. **versioning**: Semantic version calculation from commit messages, GitHub release creation

**Deployment Strategy**
- Kustomize base + overlays pattern (dev/stg/prod)
- Environment-specific configurations via ConfigMap
- Init container for SSH key setup (copy to writable volume with correct permissions)
- Resource quotas: 100m-200m CPU, 128Mi-256Mi memory
- Readiness/liveness probes on `/health` endpoint

**Secret Management**
- Kubernetes secrets for kubeconfig and SSH private key
- Mounted read-only, copied by init container
- Never logged or exposed in error messages

### Observability

**Structured Logging** (logrus)
```go
logger.WithFields(logrus.Fields{
  "sessionID": sessionID,
  "clusterID": clusterID,
  "operation": "reset",
  "duration": elapsed,
}).Info("Cluster reset completed")
```

**Metrics Exposure**
- Prometheus endpoint at `/metrics`
- Go runtime metrics (goroutines, memory, GC)
- Custom metrics: session count, cluster pool status, validation duration

**Health Checks**
- `/health` endpoint for Kubernetes probes
- VM readiness polling with fallback heuristics
- Background health monitoring (5-minute interval)

### Resilience Patterns

**Exponential Backoff Retry**
```go
RetryConfig{
  MaxRetries: 3,
  Delay: 10s,
  Backoff: 2.0  // 10s, 20s, 40s
}
```
Applied to:
- Cloud-init secret creation
- VM creation
- Join command extraction
- SSH command execution

**Graceful Degradation**
- Scenario initialization failures don't block session creation
- Failed cluster resets marked as `StatusError` requiring manual intervention
- Cleanup failures logged but don't prevent session deletion

**Resource Cleanup**
- Automatic session expiration (60-minute timeout, extendable)
- Orphaned PVC detection via regex pattern matching
- Background cleanup goroutine with graceful shutdown on SIGTERM

## Technical Innovations

### Terminal Architecture

Terminal access is delegated to [cks-terminal-mgmt](https://github.com/homelabz-eu/cks-terminal-mgmt), a dedicated microservice running on the toolz cluster. The backend resolves VM names to IPs via the KubeVirt API and returns a terminal-mgmt URL for iframe embedding:

```
POST /api/v1/sessions/:id/terminals  { target: "control-plane" }
→ { terminalUrl: "https://terminal.toolz.homelabz.eu/terminal?vmIP=10.42.0.56" }
```

The `TERMINAL_MGMT_URL` is configured via environment variable/ConfigMap.

### Cluster Pool Architecture

```
Pool State Machine:
┌─────────────────────────────────────────────────┐
│ available → locked → resetting → available      │
│               ↓                                  │
│             error (manual recovery)              │
└─────────────────────────────────────────────────┘
```

**Allocation Flow**:
1. Linear search for first available cluster (O(n) with n=3)
2. Atomic status transition to `locked`
3. Return cluster metadata (namespace, VM names, network config)
4. Session inherits pre-bootstrapped infrastructure

**Reset Flow**:
1. Mark as `resetting` (persistent write)
2. Stop VMs (20-minute timeout)
3. Delete old VirtualMachineRestore + restore-* PVCs
4. Create new VirtualMachineRestore objects
5. Wait for restore completion
6. Start VMs
7. Mark as `available`

**Performance Characteristics**:
- Session creation WITH pool: <1 second
- Session creation WITHOUT pool: 10-15 minutes
- Cluster reset: 2-3 minutes
- Concurrent sessions: Limited by pool size (3)

### Cloud-Init Template System

**Variable Substitution**:
```go
substituteEnvVars(template, map[string]string{
  "CONTROL_PLANE_VM_NAME": "cp-cluster1",
  "K8S_VERSION": "1.33.0",
  "POD_CIDR": "10.0.0.0/8",
  "JOIN_COMMAND": "kubeadm join ...",  // Extracted from control-plane
})
```

**Join Command Extraction** (worker node provisioning):
1. Wait 60s for control-plane kubelet initialization
2. SSH execute: `cat /etc/kubeadm-join-command`
3. Retry with exponential backoff on failure
4. Inject into worker cloud-init
5. Worker automatically joins cluster on boot

### Unified Validation Engine

Single interface supporting multiple validation strategies:

**Resource Existence**:
```go
kubectl get Pod nginx -n default
// Parse output for "NotFound" or error
```

**Script Execution**:
```go
// 1. Create temp script: /tmp/validation-{sessionID}-{ruleID}.sh
// 2. Execute via SSH
// 3. Parse exit code from stdout
// 4. Cleanup temp file
```

**File Content**:
```go
// 1. SSH cat /path/to/file
// 2. Check condition (contains, equals, regex)
```

Benefits:
- Consistent error handling across all types
- Extensible: New validators implement same interface
- Detailed results: expected vs actual values, error codes
- Timeout protection via context cancellation

## Cluster Bootstrap Process

Complete VM cluster creation pipeline:

**1. Golden Image Validation**
- Check PVC exists: `{GoldenImageNamespace}/{GoldenImageName}`
- Fail early if missing

**2. Control Plane Creation**
- Create cloud-init secret with variables (K8S_VERSION, POD_CIDR, VM_NAME)
- Create VirtualMachine with DataVolume (clone source: golden PVC)
- Wait for VM ready (15-minute timeout, poll every 10s)
- Ready conditions: `VM.Status.Ready=true` OR `VMI.Status.Phase=Running` for >60s

**3. Join Command Extraction**
- Wait 60s for kubelet initialization
- SSH: `cat /etc/kubeadm-join-command` (written by cloud-init)
- Retry with exponential backoff (3 attempts)

**4. Worker Node Creation**
- Create cloud-init secret with JOIN_COMMAND, CONTROL_PLANE_IP, ENDPOINT
- Create VirtualMachine (clone golden PVC)
- Wait for VM ready
- Worker automatically joins cluster via cloud-init

**5. Network Configuration**
- Pod CIDR: 10.0.0.0/8 (configurable)
- CNI installed via cloud-init (Calico/Flannel)
- Service network: Kubernetes default

## Task Configuration and Validation

### Scenario Structure

```
scenarios/
├── basic-pod-security/
│   ├── metadata.yaml          # Scenario metadata
│   ├── tasks/
│   │   ├── 01-task.md        # Task 1 (Markdown)
│   │   ├── 02-task.md        # Task 2
│   │   └── 03-task.md        # Task 3
│   ├── validation/
│   │   ├── 01-validation.yaml  # Task 1 validation rules
│   │   ├── 02-validation.yaml  # Task 2 validation rules
│   │   └── 03-validation.yaml  # Task 3 validation rules
│   └── setup/
│       └── init.yaml          # Setup steps (kubectl apply, scripts)
```

### Task Markdown Format

```markdown
# Enforce Pod Security Standards

## Description
Configure Pod Security Admission in the test-pods namespace.

## Objectives
- Create namespace with pod-security labels
- Verify enforcement with test pod
- Document security context requirements

## Step-by-Step Guide
1. Create namespace: `kubectl create namespace test-pods`
2. Label namespace: `kubectl label namespace test-pods pod-security.kubernetes.io/enforce=baseline`

## Hints
<details>
<summary>Check current labels</summary>
Use kubectl get namespace test-pods -o yaml to view current configuration.
</details>
```

Parser extracts sections by H2 headers, handles nested lists, and processes `<details>` blocks for collapsible hints.

### Validation Rules

```yaml
validation:
  - id: namespace-exists
    type: resource_exists
    resource:
      kind: Namespace
      name: test-pods
    errorMessage: "Namespace test-pods not found"

  - id: enforce-label
    type: resource_property
    resource:
      kind: Namespace
      name: test-pods
      property: .metadata.labels.pod-security\.kubernetes\.io/enforce
    condition: equals
    value: "baseline"
    errorMessage: "Pod Security enforcement label not set correctly"

  - id: verify-runtime
    type: script
    script:
      script: |
        #!/bin/bash
        kubectl run test-pod --image=nginx --namespace=test-pods 2>&1 | grep -q "violates"
        test $? -eq 0  # Should violate security policy
      target: control-plane
      successCode: 0
    errorMessage: "Pod Security Standards not enforced at runtime"
```

**Validation Execution Flow**:
1. User clicks "Validate Task" in frontend
2. POST `/api/v1/sessions/{sessionID}/tasks/{taskID}/validate`
3. SessionManager loads scenario and task
4. UnifiedValidator processes each validation rule
5. Results aggregated into ValidationResponse
6. Task status updated: "pending" → "completed" or "failed"
7. Frontend displays results with expected vs actual values

## Admin Panel Functionality

The admin dashboard provides cluster pool and session management capabilities exposed via `/api/v1/admin/*` endpoints.

### Cluster Pool Management

**GET /api/v1/admin/clusters**
```json
[
  {
    "id": "cluster1",
    "status": "locked",
    "sessionId": "abc123",
    "namespace": "cluster1",
    "lockTime": "2025-11-15T10:30:00Z",
    "lastReset": "2025-11-15T08:00:00Z"
  },
  {
    "id": "cluster2",
    "status": "available",
    "sessionId": null,
    "namespace": "cluster2"
  },
  {
    "id": "cluster3",
    "status": "resetting",
    "sessionId": null,
    "namespace": "cluster3"
  }
]
```

**POST /api/v1/admin/bootstrap-pool**
- Creates baseline clusters (cp-{clusterID} + wk-{clusterID})
- Intended for initial setup or disaster recovery
- Blocks until all 3 clusters ready (can take 30-45 minutes)

**POST /api/v1/admin/create-snapshots**
- Creates VirtualMachineSnapshot objects for all cluster VMs
- Snapshot names: `cp-cluster1-snapshot`, `wk-cluster1-snapshot`, etc.
- Waits for all snapshots to reach ReadyToUse status
- Must be run after bootstrap and before first session

**POST /api/v1/admin/release-all-clusters**
- Forces release of all locked clusters
- Triggers async reset for each cluster
- Use case: Emergency cleanup, testing, maintenance

### Session Management

**GET /api/v1/admin/sessions**
```json
[
  {
    "id": "abc123",
    "scenarioId": "basic-pod-security",
    "clusterId": "cluster1",
    "namespace": "cluster1",
    "status": "running",
    "createdAt": "2025-11-15T10:30:00Z",
    "expiresAt": "2025-11-15T11:30:00Z",
    "controlPlaneVM": "cp-cluster1",
    "workerVM": "wk-cluster1",
    "tasks": [
      {"id": "task-1", "status": "completed"},
      {"id": "task-2", "status": "pending"}
    ],
    "activeTerminals": {
      "control-plane": "abc123-control-plane",
      "worker-node": "abc123-worker-node"
    }
  }
]
```

**DELETE /api/v1/admin/sessions/{sessionID}**
- Force delete session regardless of expiration
- Releases cluster back to pool
- Triggers cluster reset

## Technology Stack

**Core**
- Go 1.24.0 (static binary compilation)
- Gin Web Framework 1.10.0 (HTTP routing, middleware)
- Kubernetes Client-Go 0.31.8 (API interactions)
- KubeVirt Client-Go 1.5.0 (VM management)

**Infrastructure**
- KubeVirt 1.5+ (VM orchestration)
- Longhorn (storage class for PVCs)
- Istio (ingress, VirtualService routing)
- Cert-Manager (automatic TLS certificates)

**Session Storage**
- Redis (go-redis/v9) for session persistence

**Observability**
- Logrus 1.9.3 (structured logging)
- Prometheus client_golang 1.19.1 (metrics)

**Utilities**
- google/uuid 1.6.0 (session IDs)

## Environment Configuration

Key environment variables:

| Variable | Purpose | Default |
|----------|---------|---------|
| `KUBERNETES_CONTEXT` | Target cluster context | `toolz` |
| `KUBECONFIG` | Path to kubeconfig | `~/.kube/config` |
| `KUBERNETES_VERSION` | K8s version for VMs | `1.33.0` |
| `VM_CPU_CORES` | CPU per VM | `2` |
| `VM_MEMORY` | Memory per VM | `2Gi` |
| `VM_STORAGE_SIZE` | Disk per VM | `10Gi` |
| `VM_STORAGE_CLASS` | Storage class | `longhorn` |
| `GOLDEN_IMAGE_NAME` | Base image PVC name | `new-golden-image-1-33-0` |
| `GOLDEN_IMAGE_NAMESPACE` | Image PVC namespace | `vm-templates` |
| `POD_CIDR` | Pod network CIDR | `10.0.0.0/8` |
| `REDIS_URL` | Redis server address | `redis.toolz.homelabz.eu:6379` |
| `REDIS_PASSWORD` | Redis authentication | `""` |
| `REDIS_DB` | Redis database number | `0` |
| `SESSION_TIMEOUT_MINUTES` | Session duration | `60` |
| `CLEANUP_INTERVAL_MINUTES` | Cleanup frequency | `5` |
| `TERMINAL_MGMT_URL` | cks-terminal-mgmt external URL | `https://terminal.toolz.homelabz.eu` |

## Deployment Architecture

**Kubernetes Manifests** (`kustomize/base/`)

**Deployment**:
- Init container: Copies SSH key from secret to writable volume with correct permissions
- Main container: Runs as UID 1001, mounts kubeconfig + SSH key
- Resources: 100m-200m CPU, 128Mi-256Mi memory
- Probes: Readiness (5s delay, 10s period), Liveness (15s delay, 20s period)

**Service**:
- Type: ClusterIP
- Port: 8080 → targetPort: 8080

**VirtualService** (Istio):
- Routes traffic from ingress gateway to service
- TLS termination via cert-manager annotation
- Path-based routing: All paths to cks-backend service

**ConfigMap**:
- Environment variables for runtime configuration
- Overlay pattern: base values, environment-specific overrides

**Secrets**:
- `cluster-secrets`: kubeconfig + SSH private key
- Mounted read-only, copied by init container

## Repository Structure

```
cks-backend/
├── src/
│   ├── cmd/server/main.go           # Application entry point
│   ├── internal/
│   │   ├── clusterpool/manager.go   # Cluster pool state machine
│   │   ├── sessions/session_manager.go  # Session lifecycle
│   │   ├── sessions/store.go           # SessionStore interface
│   │   ├── sessions/store_redis.go     # Redis implementation
│   │   ├── scenarios/scenario_manager.go  # Scenario loading
│   │   ├── validation/unified_validator.go  # Validation engine
│   │   ├── kubevirt/client.go       # KubeVirt operations
│   │   ├── controllers/             # HTTP handlers
│   │   ├── services/                # Business logic (terminal URL resolution via cks-terminal-mgmt)
│   │   └── models/models.go         # Data structures
│   ├── scenarios/                   # CKS training scenarios
│   │   ├── basic-pod-security/
│   │   ├── falco-runtime-security/
│   │   └── categories.yaml
│   └── templates/                   # VM templates
│       ├── control-plane-template.yaml
│       ├── worker-node-template.yaml
│       └── cloud-config files
├── kustomize/                       # Kubernetes deployment
│   ├── base/
│   └── overlays/{dev,stg,prod}/
├── .github/workflows/               # CI/CD pipelines
├── Dockerfile                       # Multi-stage build
└── go.mod                          # Dependencies
```

## Performance Characteristics

**Session Operations**:
- Create (with pool): <1 second
- Create (bootstrap): 10-15 minutes
- Extend: <100ms
- Delete: 1-2 seconds

**Cluster Operations**:
- Bootstrap: 15-20 minutes
- Reset (with snapshots): 2-3 minutes
- Snapshot creation: 3-5 minutes
- Assignment: <10ms

**Validation**:
- Resource check: 1-2 seconds
- Command execution: 2-5 seconds
- Script validation: 3-10 seconds

**Resource Usage**:
- Backend pod: ~80Mi memory, ~50m CPU (idle)
- Per cluster: 4 cores, 4Gi memory, 20Gi storage
- Max concurrent sessions: 3 (pool size)

## Security Implementation

**Container Security**:
- Non-root execution (UID 1001)
- Static binary (no runtime dependencies)
- Minimal base image (Alpine 3.19)
- Health checks for liveness/readiness

**Secret Management**:
- SSH keys in Kubernetes secrets
- Kubeconfig in secrets
- Init container for permission setup
- Never logged or exposed

**Network Isolation**:
- Namespace per session
- Resource quotas prevent resource exhaustion
- Istio service mesh for mTLS (if configured)

**SSH Security**:
- Known hosts checking disabled for VM connections
- Private key mounted read-only
- Temp kubeconfig with restricted permissions

