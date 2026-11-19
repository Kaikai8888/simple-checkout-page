package app

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"

	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
	"checkout/internal/infra/cache"
	"checkout/internal/infra/database/repo"
)

type AuthService struct {
	customers *repo.CustomerRepo
	sessions  *cache.SessionStore
	limiter   *cache.LoginLimiter
	// dummyHash is compared against when the email does not exist, so "unknown email" and
	// "wrong password" take the same time and cannot be told apart (no account enumeration).
	dummyHash []byte
}

func NewAuthService(customers *repo.CustomerRepo, sessions *cache.SessionStore, limiter *cache.LoginLimiter) *AuthService {
	dummy, _ := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)
	return &AuthService{customers: customers, sessions: sessions, limiter: limiter, dummyHash: dummy}
}

// Register creates a customer account and logs it in.
func (s *AuthService) Register(ctx context.Context, in adapter.RegisterInput) (adapter.LoginOutput, error) {
	email := model.NormalizeEmail(in.Email)
	if err := model.ValidateRegistration(email, in.Password, in.Name); err != nil {
		return adapter.LoginOutput{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return adapter.LoginOutput{}, err
	}
	user, err := s.customers.Create(ctx, email, in.Name, string(hash), def.RoleCustomer)
	if err != nil {
		return adapter.LoginOutput{}, err
	}
	return s.startSession(ctx, user)
}

func (s *AuthService) Login(ctx context.Context, in adapter.LoginInput) (adapter.LoginOutput, error) {
	email := model.NormalizeEmail(in.Email)
	if blocked, err := s.limiter.Blocked(ctx, email); err != nil {
		return adapter.LoginOutput{}, err
	} else if blocked {
		return adapter.LoginOutput{}, def.NewError(def.ErrTooManyAttempts, "登入失敗次數過多，請 15 分鐘後再試")
	}

	user, hash, err := s.customers.GetByEmail(ctx, email)
	if err != nil {
		return adapter.LoginOutput{}, err
	}
	if user == nil {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(in.Password))
		return adapter.LoginOutput{}, s.loginFailed(ctx, email)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		return adapter.LoginOutput{}, s.loginFailed(ctx, email)
	}
	if err := s.limiter.Reset(ctx, email); err != nil {
		slog.WarnContext(ctx, "reset login limiter failed", "err", err)
	}
	return s.startSession(ctx, *user)
}

func (s *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.sessions.Delete(ctx, token)
}

// Authenticate resolves a session token to the logged-in user.
func (s *AuthService) Authenticate(ctx context.Context, token string) (model.User, error) {
	if token == "" {
		return model.User{}, def.NewError(def.ErrUnauthorized, "請先登入")
	}
	u, err := s.sessions.Get(ctx, token)
	if err != nil {
		return model.User{}, err
	}
	if u == nil {
		return model.User{}, def.NewError(def.ErrUnauthorized, "登入已過期，請重新登入")
	}
	return *u, nil
}

// EnsureAdmin bootstraps the admin account at startup (demo convenience; credentials come from env).
func (s *AuthService) EnsureAdmin(ctx context.Context, email, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.customers.EnsureAdmin(ctx, model.NormalizeEmail(email), "Admin", string(hash))
}

func (s *AuthService) SessionTTL() time.Duration { return s.sessions.TTL() }

func (s *AuthService) startSession(ctx context.Context, u model.User) (adapter.LoginOutput, error) {
	token, err := s.sessions.Create(ctx, u)
	if err != nil {
		return adapter.LoginOutput{}, err
	}
	return adapter.LoginOutput{User: u, SessionToken: token}, nil
}

func (s *AuthService) loginFailed(ctx context.Context, email string) error {
	if err := s.limiter.RecordFailure(ctx, email); err != nil {
		slog.WarnContext(ctx, "record login failure failed", "err", err)
	}
	return def.NewError(def.ErrInvalidLogin, "email 或密碼錯誤")
}
