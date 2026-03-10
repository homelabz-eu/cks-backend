// backend/internal/services/scenario_service.go

package services

import (
	"github.com/homelabz-eu/cks/backend/internal/models"
	"github.com/homelabz-eu/cks/backend/internal/scenarios"
)

// ScenarioServiceImpl implements the ScenarioService interface
type ScenarioServiceImpl struct {
	scenarioManager *scenarios.ScenarioManager
}

// NewScenarioService creates a new scenario service
func NewScenarioService(scenarioManager *scenarios.ScenarioManager) ScenarioService {
	return &ScenarioServiceImpl{
		scenarioManager: scenarioManager,
	}
}

// GetScenario returns a scenario by ID
func (s *ScenarioServiceImpl) GetScenario(id string) (*models.Scenario, error) {
	return s.scenarioManager.GetScenario(id)
}

// ListScenarios returns a list of scenarios
func (s *ScenarioServiceImpl) ListScenarios(category, difficulty, searchQuery string) ([]*models.Scenario, error) {
	return s.scenarioManager.ListScenarios(category, difficulty, searchQuery)
}

// GetCategories returns all scenario categories
func (s *ScenarioServiceImpl) GetCategories() (map[string]string, error) {
	return s.scenarioManager.GetCategories()
}

func (s *ScenarioServiceImpl) ReloadScenarios() error {
	return s.scenarioManager.ReloadScenarios()
}
