package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/homelabz-eu/cks/backend/internal/clusterpool"
	"github.com/homelabz-eu/cks/backend/internal/config"
	"github.com/homelabz-eu/cks/backend/internal/controllers"
	"github.com/homelabz-eu/cks/backend/internal/kubevirt"
	"github.com/homelabz-eu/cks/backend/internal/middleware"
	"github.com/homelabz-eu/cks/backend/internal/scenarios"
	"github.com/homelabz-eu/cks/backend/internal/services"
	"github.com/homelabz-eu/cks/backend/internal/sessions"
	"github.com/homelabz-eu/cks/backend/internal/validation"
)

// createKubernetesConfig  creates Kubernetes config with explicit context selection
func createKubernetesConfig(cfg *config.Config, logger *logrus.Logger) (*rest.Config, error) {
	var k8sConfig *rest.Config
	var err error

	logger.WithFields(logrus.Fields{
		"kubeconfigPath":    cfg.KubeconfigPath,
		"kubernetesContext": cfg.KubernetesContext,
	}).Info("Creating Kubernetes client configuration")

	if cfg.KubeconfigPath != "" {
		// Load kubeconfig and explicitly set context
		k8sConfig, err = createConfigWithContext(cfg.KubeconfigPath, cfg.KubernetesContext, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to build kubeconfig with context: %w", err)
		}
	} else {
		// Use in-cluster configuration
		logger.Info("Using in-cluster Kubernetes configuration")
		k8sConfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to create in-cluster config: %w", err)
		}
	}

	return k8sConfig, nil
}

// createConfigWithContext creates a Kubernetes config using specific context
func createConfigWithContext(kubeconfigPath, contextName string, logger *logrus.Logger) (*rest.Config, error) {
	// Load the kubeconfig file
	configLoader := &clientcmd.ClientConfigLoadingRules{
		ExplicitPath: kubeconfigPath,
	}

	// Load raw config
	rawConfig, err := configLoader.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig from %s: %w", kubeconfigPath, err)
	}

	// Validate that the desired context exists
	if _, exists := rawConfig.Contexts[contextName]; !exists {
		availableContexts := make([]string, 0, len(rawConfig.Contexts))
		for ctx := range rawConfig.Contexts {
			availableContexts = append(availableContexts, ctx)
		}

		logger.WithFields(logrus.Fields{
			"requestedContext":  contextName,
			"availableContexts": availableContexts,
		}).Error("Requested Kubernetes context not found")

		return nil, fmt.Errorf("context '%s' not found in kubeconfig. Available contexts: %v", contextName, availableContexts)
	}

	// Override the current context
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: contextName,
	}

	// Create client config with explicit context
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		configLoader,
		configOverrides,
	)

	// Build REST config
	config, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create client config for context '%s': %w", contextName, err)
	}

	// Log successful context selection
	logger.WithFields(logrus.Fields{
		"context": contextName,
		"server":  config.Host,
	}).Info("Successfully created Kubernetes config with explicit context")

	return config, nil
}

func main() {
	logger := logrus.New()
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		logger.WithError(err).Fatal("Failed to load configuration")
	}

	// Configure formatter based on config
	switch cfg.LogFormat {
	case "json":
		logger.SetFormatter(&logrus.JSONFormatter{})
	case "text":
		logger.SetFormatter(&logrus.TextFormatter{
			FullTimestamp: true,
			DisableColors: false, // Enable colors for development
		})
	default:
		logger.SetFormatter(&logrus.TextFormatter{
			FullTimestamp: true,
			DisableColors: false,
		})
	}

	// Set log level based on configuration
	logLevel, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.WithError(err).Warn("Invalid log level, using info")
	}
	logger.SetLevel(logLevel)

	// Set up Gin
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create Gin router without default middleware
	router := gin.New()

	// Add recovery middleware
	router.Use(gin.Recovery())

	// Add custom logging middleware that skips health checks
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		SkipPaths: []string{"/health", "/metrics"},
	}))

	// Configure middleware
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{cfg.CorsAllowOrigin},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))
	router.Use(middleware.RequestID())
	router.Use(middleware.Logger())

	// Metrics endpoint (health check registered after Redis init)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Create Kubernetes client configuration with explicit context
	k8sConfig, err := createKubernetesConfig(cfg, logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create Kubernetes configuration")
	}

	// Create Kubernetes client
	kubeClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create kubernetes client")
	}

	// Create KubeVirt client
	kubevirtClient, err := kubevirt.NewClient(k8sConfig, logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create kubevirt client")
	}

	unifiedValidator := validation.NewUnifiedValidator(kubevirtClient, logger)

	// Create scenario manager first
	scenarioManager, err := scenarios.NewScenarioManager(cfg.ScenariosPath, logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create scenario manager")
	}

	// Create cluster pool manager
	clusterPoolManager, err := clusterpool.NewManager(cfg, kubeClient, kubevirtClient, logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create cluster pool manager")
	}

	// Initialize Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr:            cfg.RedisURL,
		Password:        cfg.RedisPassword,
		DB:              cfg.RedisDB,
		MaxRetries:      3,
		MinRetryBackoff: 100 * time.Millisecond,
		MaxRetryBackoff: 2 * time.Second,
		DialTimeout:     5 * time.Second,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
	})

	redisPingCtx, redisPingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer redisPingCancel()
	if err := redisClient.Ping(redisPingCtx).Err(); err != nil {
		logger.WithError(err).Fatal("Failed to connect to Redis")
	}
	logger.WithField("addr", cfg.RedisURL).Info("Redis connection established")

	// Health check (registered after Redis init)
	router.GET("/health", func(c *gin.Context) {
		healthCtx, healthCancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer healthCancel()
		if err := redisClient.Ping(healthCtx).Err(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unhealthy",
				"redis":  "disconnected",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "redis": "connected"})
	})

	sessionStore := sessions.NewRedisSessionStore(redisClient, logger)

	sessionManager, err := sessions.NewSessionManager(cfg, kubeClient, kubevirtClient, unifiedValidator, logger, scenarioManager, clusterPoolManager, sessionStore)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create session manager")
	}

	// Create service layer implementations
	sessionService := services.NewSessionService(sessionManager)
	terminalService := services.NewTerminalService(kubevirtClient, cfg)
	scenarioService := services.NewScenarioService(scenarioManager)

	// Create and register controllers
	sessionController := controllers.NewSessionController(sessionService, scenarioService, logger, unifiedValidator)
	sessionController.RegisterRoutes(router)

	terminalController := controllers.NewTerminalController(terminalService, sessionService, logger)
	terminalController.RegisterRoutes(router)

	scenarioController := controllers.NewScenarioController(scenarioService)
	scenarioController.RegisterRoutes(router)

	adminController := controllers.NewAdminController(sessionManager, kubevirtClient, logger)
	adminController.RegisterRoutes(router)

	// Create HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.ServerHost, cfg.ServerPort),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Run server in a goroutine
	go func() {
		logger.WithFields(logrus.Fields{
			"host": cfg.ServerHost,
			"port": cfg.ServerPort,
		}).Info("Starting server")

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.WithError(err).Fatal("Failed to start server")
		}
	}()

	// Wait for interrupt signal to gracefully shut down the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("Shutting down server...")

	// Create context with timeout for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stop cluster pool manager
	clusterPoolManager.Stop()

	// Close Redis connection
	redisClient.Close()

	// Shutdown server
	if err := server.Shutdown(ctx); err != nil {
		logger.WithError(err).Fatal("Server forced to shutdown")
	}

	logger.Info("Server exited properly")
}
