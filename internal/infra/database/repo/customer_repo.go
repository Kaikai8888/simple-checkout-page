package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

const pgUniqueViolation = "23505"

type CustomerRepo struct {
	pool *pgxpool.Pool
}

func NewCustomerRepo(pool *pgxpool.Pool) *CustomerRepo {
	return &CustomerRepo{pool: pool}
}

// Create inserts an account. Returns EMAIL_TAKEN if the email is already registered.
func (r *CustomerRepo) Create(ctx context.Context, email, name, passwordHash, role string) (model.User, error) {
	u := model.User{Email: email, Name: name, Role: role}
	err := r.pool.QueryRow(ctx, `
		INSERT INTO customers (email, name, password_hash, role) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, name, passwordHash, role).Scan(&u.ID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return model.User{}, def.NewError(def.ErrEmailTaken, "此 email 已註冊，請直接登入")
	}
	if err != nil {
		return model.User{}, fmt.Errorf("insert customer: %w", err)
	}
	return u, nil
}

// GetByEmail returns the account and its password hash; nil when not found.
func (r *CustomerRepo) GetByEmail(ctx context.Context, email string) (*model.User, string, error) {
	var u model.User
	var hash string
	err := r.pool.QueryRow(ctx, "SELECT id, email, name, role, password_hash FROM customers WHERE email = $1", email).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("get customer: %w", err)
	}
	return &u, hash, nil
}

// EnsureAdmin creates the admin account if missing, or promotes an existing account to admin.
// The password is only set on creation, so an existing admin's password is never overwritten.
func (r *CustomerRepo) EnsureAdmin(ctx context.Context, email, name, passwordHash string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO customers (email, name, password_hash, role) VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE SET role = EXCLUDED.role, updated_at = now()`,
		email, name, passwordHash, def.RoleAdmin)
	return err
}
