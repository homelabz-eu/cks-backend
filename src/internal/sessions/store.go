package sessions

import (
	"context"

	"github.com/homelabz-eu/cks/backend/internal/models"
)

type SessionStore interface {
	Get(ctx context.Context, sessionID string) (*models.Session, error)
	Save(ctx context.Context, session *models.Session) error
	Delete(ctx context.Context, sessionID string) error
	List(ctx context.Context) ([]*models.Session, error)
	Count(ctx context.Context) (int, error)
}
