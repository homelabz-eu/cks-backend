package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"

	"github.com/homelabz-eu/cks-backend/internal/models"
)

const (
	sessionKeyPrefix = "cks:session:"
	sessionIndexKey  = "cks:sessions"
	ttlBuffer        = 5 * time.Minute
)

type RedisSessionStore struct {
	client *redis.Client
	logger *logrus.Logger
}

func NewRedisSessionStore(client *redis.Client, logger *logrus.Logger) *RedisSessionStore {
	return &RedisSessionStore{
		client: client,
		logger: logger,
	}
}

func sessionKey(id string) string {
	return sessionKeyPrefix + id
}

func (s *RedisSessionStore) Get(ctx context.Context, sessionID string) (*models.Session, error) {
	data, err := s.client.Get(ctx, sessionKey(sessionID)).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session from redis: %w", err)
	}

	var session models.Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &session, nil
}

func (s *RedisSessionStore) Save(ctx context.Context, session *models.Session) error {
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	ttl := time.Until(session.ExpirationTime) + ttlBuffer
	if ttl <= 0 {
		ttl = ttlBuffer
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, sessionKey(session.ID), data, ttl)
	pipe.SAdd(ctx, sessionIndexKey, session.ID)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to save session to redis: %w", err)
	}

	return nil
}

func (s *RedisSessionStore) Delete(ctx context.Context, sessionID string) error {
	pipe := s.client.Pipeline()
	pipe.Del(ctx, sessionKey(sessionID))
	pipe.SRem(ctx, sessionIndexKey, sessionID)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete session from redis: %w", err)
	}

	return nil
}

func (s *RedisSessionStore) List(ctx context.Context) ([]*models.Session, error) {
	ids, err := s.client.SMembers(ctx, sessionIndexKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list session ids from redis: %w", err)
	}

	if len(ids) == 0 {
		return []*models.Session{}, nil
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = sessionKey(id)
	}

	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get sessions from redis: %w", err)
	}

	sessions := make([]*models.Session, 0, len(values))
	staleIDs := make([]interface{}, 0)

	for i, val := range values {
		if val == nil {
			staleIDs = append(staleIDs, ids[i])
			continue
		}

		strVal, ok := val.(string)
		if !ok {
			continue
		}

		var session models.Session
		if err := json.Unmarshal([]byte(strVal), &session); err != nil {
			s.logger.WithError(err).WithField("sessionID", ids[i]).Warn("Failed to unmarshal session, removing stale entry")
			staleIDs = append(staleIDs, ids[i])
			continue
		}

		sessions = append(sessions, &session)
	}

	if len(staleIDs) > 0 {
		s.client.SRem(ctx, sessionIndexKey, staleIDs...)
	}

	return sessions, nil
}

func (s *RedisSessionStore) Count(ctx context.Context) (int, error) {
	count, err := s.client.SCard(ctx, sessionIndexKey).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to count sessions in redis: %w", err)
	}

	return int(count), nil
}
