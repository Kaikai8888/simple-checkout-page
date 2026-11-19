package web

import (
	"context"
	"mime"
	"net/http"

	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

const sessionCookie = "sid"

type userCtxKey struct{}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in adapter.RegisterInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.auth.Register(r.Context(), in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, out.SessionToken)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in adapter.LoginInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.auth.Login(r.Context(), in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, out.SessionToken)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), c.Value); err != nil {
			writeError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cookieSecure})
	writeJSON(w, http.StatusOK, map[string]string{"result": "OK"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": userFrom(r)})
}

// requireUser rejects requests without a valid session and puts the user into the request context.
//
// CSRF: the cookie is SameSite=Lax, so browsers do not send it on cross-site POST/PUT. As a second layer,
// state-changing requests must be application/json, which a plain cross-site HTML form cannot send.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeError(w, r, def.NewError(def.ErrInvalidRequest, "Content-Type must be application/json"))
				return
			}
		}
		token := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
		user, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			writeError(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, user)))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r).IsAdmin() {
			writeError(w, r, def.NewError(def.ErrForbidden, "需要管理員權限"))
			return
		}
		next(w, r)
	})
}

// userFrom must only be called from handlers wrapped by requireUser.
func userFrom(r *http.Request) model.User {
	u, _ := r.Context().Value(userCtxKey{}).(model.User)
	return u
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.auth.SessionTTL().Seconds()),
		HttpOnly: true,                 // not readable from JS -> an XSS cannot steal the session
		SameSite: http.SameSiteLaxMode, // not sent on cross-site POST -> CSRF protection
		Secure:   s.cookieSecure,       // true behind HTTPS in production
	})
}
