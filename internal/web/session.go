package web

import (
	"context"
	"net/http"
	"time"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/store"
)

const (
	sessionCookieName = "avas_session"
	betslipCookieName = "avas_slip"
	sessionLifetime   = 12 * time.Hour
)

// resolveSession loads and validates a session cookie.
func (s *Server) resolveSession(ctx context.Context, token string) (store.Session, store.User, bool) {
	session, err := s.store.GetSession(ctx, auth.HashToken(token))
	if err != nil {
		return store.Session{}, store.User{}, false
	}
	if !s.now().Before(session.ExpiresAt) {
		_ = s.store.DeleteSession(ctx, session.TokenHash)
		return store.Session{}, store.User{}, false
	}
	user, err := s.store.GetUser(ctx, session.UserID)
	if err != nil {
		return store.Session{}, store.User{}, false
	}
	// A suspended or closed account must lose access immediately, not when its
	// cookie happens to expire.
	if user.Status != store.UserActive {
		_ = s.store.DeleteUserSessions(ctx, user.ID)
		return store.Session{}, store.User{}, false
	}
	return session, user, true
}

// startSession issues a session cookie for a customer.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) (string, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	if err := s.store.CreateSession(r.Context(), store.Session{
		TokenHash:      auth.HashToken(token),
		UserID:         userID,
		CreatedAt:      now,
		ExpiresAt:      now.Add(sessionLifetime),
		LastSeenAt:     now,
		RealityCheckAt: s.compliance.NextRealityCheck(),
		IP:             clientIP(r),
		UserAgent:      truncate(r.UserAgent(), 255),
	}); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.IsProduction(),
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(sessionLifetime),
	})
	return token, nil
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// csrfToken derives this session's CSRF token from the raw cookie value, which
// is held in the request context and never written to the database.
func (s *Server) csrfToken(ctx context.Context) string {
	token, ok := ctx.Value(ctxToken).(string)
	if !ok {
		return ""
	}
	return auth.CSRFToken(s.cfg.SecretKey, token)
}

// checkCSRF verifies a submitted form token. Every state-changing handler
// calls it before doing anything else.
func (s *Server) checkCSRF(r *http.Request) bool {
	token, ok := r.Context().Value(ctxToken).(string)
	if !ok {
		return false
	}
	return auth.CheckCSRF(s.cfg.SecretKey, token, r.FormValue("csrf"))
}

// ---------------------------------------------------------------------------
// Route guards
// ---------------------------------------------------------------------------

// requireCustomer refuses anonymous requests and applies the checks that must
// hold on every authenticated page: the account has to be usable, and a due
// reality check has to be acknowledged before play continues.
func (s *Server) requireCustomer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := CurrentUser(r.Context())
		if !ok {
			s.redirectToLogin(w, r)
			return
		}

		// Withdrawing and reading your own account stay reachable while
		// excluded; placing a bet does not. The compliance layer draws that
		// line, so only the play surfaces are gated here.
		if isPlaySurface(r.URL.Path) {
			if err := s.compliance.CheckAccountUsable(r.Context(), user); err != nil {
				s.renderRefusal(w, r, err)
				return
			}
			if session, ok := CurrentSession(r.Context()); ok && s.compliance.RealityCheckDue(session) {
				if r.URL.Path != "/reality-check" {
					http.Redirect(w, r, "/reality-check", http.StatusSeeOther)
					return
				}
			}
		}
		next(w, r)
	}
}

// isPlaySurface reports whether a path is one a self-excluded customer must
// not reach. Money out and account settings are deliberately excluded: a
// customer who has stopped playing still needs to reach their own funds.
func isPlaySurface(path string) bool {
	switch {
	case path == "/betslip" || hasPrefix(path, "/betslip/"):
		return true
	case path == "/bets":
		return true
	case path == "/poker" || hasPrefix(path, "/poker/"):
		// Poker is gambling, so a self-excluded customer must not reach a
		// table. Reading a published hand history is not gambling, so hand
		// pages and the verifier stay open.
		return !hasPrefix(path, "/poker/hands/") && path != "/poker/verify"
	case path == "/reality-check":
		return true
	default:
		return false
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func (s *Server) requireStaff(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(next, func(user store.User) bool { return user.IsStaff() })
}

func (s *Server) requireTrader(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(next, func(user store.User) bool { return user.CanTrade() })
}

func (s *Server) requireCompliance(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(next, func(user store.User) bool { return user.CanReviewCompliance() })
}

func (s *Server) requireRole(next http.HandlerFunc, allowed func(store.User) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := CurrentUser(r.Context())
		if !ok {
			s.redirectToLogin(w, r)
			return
		}
		if !allowed(user) {
			// Not "forbidden, and here is what you are missing": an
			// unprivileged account is told the page does not exist.
			s.renderError(w, r, http.StatusNotFound, "Page not found")
			return
		}
		next(w, r)
	}
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Path
	if r.Method != http.MethodGet {
		next = "/"
	}
	http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
}

// renderRefusal shows a compliance refusal as a page the customer can read.
func (s *Server) renderRefusal(w http.ResponseWriter, r *http.Request, err error) {
	refusal, ok := compliance.AsRefusal(err)
	if !ok {
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	status := http.StatusForbidden
	if refusal.Code == compliance.CodeInvalidInput {
		status = http.StatusBadRequest
	}
	s.render(w, r, status, "refusal.html", map[string]any{
		"Title":   "We cannot accept that",
		"Refusal": refusal,
	})
}

func clientIP(r *http.Request) string {
	// The edge is trusted to set this; it is used for audit records, never for
	// an authorisation decision.
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if comma := indexByte(forwarded, ','); comma > 0 {
			return trimSpace(forwarded[:comma])
		}
		return trimSpace(forwarded)
	}
	return r.RemoteAddr
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
