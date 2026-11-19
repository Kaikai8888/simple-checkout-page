package model

import (
	"net/mail"
	"strings"
	"unicode/utf8"

	"checkout/internal/domain/def"
)

const (
	minPasswordLength = 8
	maxPasswordBytes  = 72 // bcrypt ignores anything longer
)

// User is the logged-in account attached to a request.
type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

func (u User) IsAdmin() bool { return u.Role == def.RoleAdmin }

// NormalizeEmail lower-cases and trims so "A@x.com" and "a@x.com " are the same account.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func ValidateRegistration(email, password, name string) *def.AppError {
	var problems []string
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		problems = append(problems, "email 格式不正確")
	}
	if utf8.RuneCountInString(password) < minPasswordLength {
		problems = append(problems, "密碼至少需要 8 個字元")
	}
	if len(password) > maxPasswordBytes {
		problems = append(problems, "密碼過長")
	}
	if strings.TrimSpace(name) == "" {
		problems = append(problems, "請填寫姓名")
	}
	if len(problems) > 0 {
		return def.NewError(def.ErrInvalidRequest, "註冊資料不正確").WithDetails(problems)
	}
	return nil
}
