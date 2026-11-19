package cache

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"checkout/internal/domain/model"
)

// SessionStore keeps server-side sessions in Valkey: session:{token} -> user JSON.
// Opaque random tokens (instead of JWT) can be revoked instantly on logout.
type SessionStore struct {
	client *redis.Client
	ttl    time.Duration
}

func NewSessionStore(client *redis.Client, ttl time.Duration) *SessionStore {
	return &SessionStore{client: client, ttl: ttl}
}

func sessionKey(token string) string { return "session:" + token }

func (s *SessionStore) Create(ctx context.Context, u model.User) (string, error) {
	b := make([]byte, 32) // 256 bits of entropy
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	payload, err := json.Marshal(u)
	if err != nil {
		return "", err
	}
	if err := s.client.Set(ctx, sessionKey(token), payload, s.ttl).Err(); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// Get returns nil when the session does not exist or expired.
func (s *SessionStore) Get(ctx context.Context, token string) (*model.User, error) {
	raw, err := s.client.Get(ctx, sessionKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	var u model.User
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, nil
	}
	return &u, nil
}

func (s *SessionStore) Delete(ctx context.Context, token string) error {
	return s.client.Del(ctx, sessionKey(token)).Err()
}

func (s *SessionStore) TTL() time.Duration { return s.ttl }

// LoginLimiter counts failed logins per email in a fixed window to slow down password guessing.
type LoginLimiter struct {
	client      *redis.Client
	maxFailures int64
	window      time.Duration
}

func NewLoginLimiter(client *redis.Client, maxFailures int64, window time.Duration) *LoginLimiter {
	return &LoginLimiter{client: client, maxFailures: maxFailures, window: window}
}

func limiterKey(email string) string { return "login_fail:" + email }

func (l *LoginLimiter) Blocked(ctx context.Context, email string) (bool, error) {
	n, err := l.client.Get(ctx, limiterKey(email)).Int64()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	return n >= l.maxFailures, err
}

func (l *LoginLimiter) RecordFailure(ctx context.Context, email string) error {
	pipe := l.client.TxPipeline()
	pipe.Incr(ctx, limiterKey(email))
	pipe.ExpireNX(ctx, limiterKey(email), l.window) // window starts at the first failure
	_, err := pipe.Exec(ctx)
	return err
}

func (l *LoginLimiter) Reset(ctx context.Context, email string) error {
	return l.client.Del(ctx, limiterKey(email)).Err()
}
