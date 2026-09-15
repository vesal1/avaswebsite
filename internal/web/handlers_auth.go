package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vesal1/avaswebsite/internal/auth"
	"github.com/vesal1/avaswebsite/internal/compliance"
	"github.com/vesal1/avaswebsite/internal/store"
)

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := CurrentUser(r.Context()); ok {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "register.html", map[string]any{
		"Title":     "Open an account",
		"Country":   Country(r.Context()),
		"MinAge":    s.cfg.MinimumAge,
		"MinLength": auth.MinPasswordLength,
		"Wide":      true,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That form could not be read.")
		return
	}

	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	dateOfBirth := strings.TrimSpace(r.FormValue("date_of_birth"))

	// The country comes from geolocation, never from the form: letting a
	// customer type their own jurisdiction would make the licence check
	// meaningless.
	country := Country(ctx)

	fail := func(message string) {
		s.render(w, r, http.StatusBadRequest, "register.html", map[string]any{
			"Title": "Open an account", "Error": message, "Country": country,
			"MinAge": s.cfg.MinimumAge, "MinLength": auth.MinPasswordLength,
			"Email": email, "DisplayName": displayName, "DateOfBirth": dateOfBirth,
			"Wide": true,
		})
	}

	if email == "" || !strings.Contains(email, "@") {
		fail("Enter a valid email address.")
		return
	}
	if displayName == "" {
		displayName = strings.SplitN(email, "@", 2)[0]
	}
	if r.FormValue("terms") == "" {
		fail("You need to accept the betting rules to open an account.")
		return
	}

	if err := s.compliance.CheckRegistration(country, dateOfBirth); err != nil {
		if refusal, ok := compliance.AsRefusal(err); ok {
			fail(refusal.Message)
			return
		}
		s.serverError(w, r, err)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		if errors.Is(err, auth.ErrWeakPassword) {
			fail(strings.TrimPrefix(err.Error(), "auth: "))
			return
		}
		s.serverError(w, r, err)
		return
	}

	userID, err := s.store.CreateUser(ctx, store.NewUser{
		Email: email, PasswordHash: hash, DisplayName: displayName,
		DateOfBirth: dateOfBirth, Country: country,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail("An account with that email address already exists.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	if err := s.store.Audit(ctx, store.AuditEntry{
		SubjectUserID: userID, Action: "account_opened",
		Detail: "country " + country, IP: clientIP(r),
	}); err != nil {
		s.log.Error("audit registration", "error", err)
	}
	if _, err := s.startSession(w, r, userID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?welcome=1", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := CurrentUser(r.Context()); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.html", map[string]any{
		"Title": "Sign in", "Next": safeNext(r.URL.Query().Get("next")), "Wide": true,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That form could not be read.")
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))

	fail := func(message string) {
		_ = s.store.RecordLoginAttempt(ctx, email, clientIP(r), false)
		// Deliberately the same message whether the account exists or not:
		// a distinct "no such account" turns the form into an account
		// enumeration oracle.
		s.render(w, r, http.StatusUnauthorized, "login.html", map[string]any{
			"Title": "Sign in", "Error": message, "Email": email, "Next": next, "Wide": true,
		})
	}

	failures, err := s.store.FailedLoginsSince(ctx, email, s.now().Add(-auth.ThrottleWindow))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if failures >= auth.MaxFailedAttempts {
		s.render(w, r, http.StatusTooManyRequests, "login.html", map[string]any{
			"Title": "Sign in", "Next": next, "Wide": true,
			"Error": "Too many failed sign-in attempts. Try again in " +
				auth.FormatDuration(auth.ThrottleWindow) + ".",
		})
		return
	}

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		fail("Email or password is incorrect.")
		return
	}
	if err := auth.VerifyPassword(password, user.PasswordHash); err != nil {
		fail("Email or password is incorrect.")
		return
	}
	if user.Status != store.UserActive {
		fail("This account is not currently active. Contact " + s.cfg.SupportMail + ".")
		return
	}

	_ = s.store.RecordLoginAttempt(ctx, email, clientIP(r), true)
	if err := s.store.TouchLogin(ctx, user.ID); err != nil {
		s.log.Error("touch login", "error", err)
	}
	if _, err := s.startSession(w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.Audit(ctx, store.AuditEntry{
		ActorUserID: user.ID, SubjectUserID: user.ID, Action: "sign_in", IP: clientIP(r),
	}); err != nil {
		s.log.Error("audit sign in", "error", err)
	}

	// A customer who signed in while excluded is told immediately rather than
	// discovering it at the bet slip.
	if err := s.compliance.CheckAccountUsable(ctx, user); err != nil {
		s.renderRefusal(w, r, err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.renderError(w, r, http.StatusForbidden, "That request could not be verified. Try again.")
		return
	}
	if session, ok := CurrentSession(r.Context()); ok {
		if err := s.store.DeleteSession(r.Context(), session.TokenHash); err != nil {
			s.log.Error("delete session", "error", err)
		}
	}
	clearSessionCookie(w, s.cfg.IsProduction())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeNext keeps post-login redirects on this site. An absolute or
// protocol-relative URL here would be an open redirect.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	if strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}
