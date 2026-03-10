// backend/internal/controllers/scenario_controller.go

package controllers

import (
	"net/http"

	"github.com/homelabz-eu/cks-backend/internal/models"
	"github.com/homelabz-eu/cks-backend/internal/services"
	"github.com/gin-gonic/gin"
)

// ScenarioController handles HTTP requests related to scenarios
type ScenarioController struct {
	scenarioService services.ScenarioService
}

// NewScenarioController creates a new scenario controller
func NewScenarioController(scenarioService services.ScenarioService) *ScenarioController {
	return &ScenarioController{
		scenarioService: scenarioService,
	}
}

// RegisterRoutes registers the scenario controller routes
func (sc *ScenarioController) RegisterRoutes(router *gin.Engine) {
	scenarios := router.Group("/api/v1/scenarios")
	{
		scenarios.GET("", sc.ListScenarios)
		scenarios.GET("/:id", sc.GetScenario)
		scenarios.GET("/categories", sc.ListCategories)
		scenarios.POST("/reload", sc.ReloadScenarios)
		scenarios.GET("/:id/tasks/:taskId/validation", sc.GetTaskValidation)

	}
}

// ListScenarios returns a list of all available scenarios
func (sc *ScenarioController) ListScenarios(c *gin.Context) {
	// Get query parameters for filtering
	category := c.Query("category")
	difficulty := c.Query("difficulty")
	searchQuery := c.Query("search")

	// Get scenarios with filters
	scenarios, err := sc.scenarioService.ListScenarios(category, difficulty, searchQuery)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, scenarios)
}

// GetScenario returns details for a specific scenario
func (sc *ScenarioController) GetScenario(c *gin.Context) {
	scenarioID := c.Param("id")

	scenario, err := sc.scenarioService.GetScenario(scenarioID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, scenario)
}

// ListCategories returns all available scenario categories
func (sc *ScenarioController) ListCategories(c *gin.Context) {
	categories, err := sc.scenarioService.GetCategories()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, categories)
}

// ReloadScenarios handles scenario reloading
func (sc *ScenarioController) ReloadScenarios(c *gin.Context) {
	err := sc.scenarioService.ReloadScenarios()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Scenarios reloaded"})
}

// GetTaskValidation returns validation rules for a specific task
func (sc *ScenarioController) GetTaskValidation(c *gin.Context) {
	scenarioID := c.Param("id")
	taskID := c.Param("taskId")

	scenario, err := sc.scenarioService.GetScenario(scenarioID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// Find the task
	var task *models.Task
	for _, t := range scenario.Tasks {
		if t.ID == taskID {
			task = &t
			break
		}
	}

	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"taskId":     task.ID,
		"validation": task.Validation,
	})
}
