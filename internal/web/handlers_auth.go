package web

import (
	"net/http"

	"github.com/leqwin/monbooru/internal/logx"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.EnablePassword {
		// Render the login form with an inline notice and the input/submit
		// disabled instead of silently redirecting - a user who bookmarked
		// /login after disabling auth otherwise gets no explanation for why
		// the page vanished, and leaving the fields live makes it look like
		// the 'login' somehow worked when the server just redirects to /.
		s.renderTemplate(w, "login.html", map[string]any{
			"CSRFToken":    s.csrfToken("anon"),
			"Error":        "Password authentication is disabled. Enable it from Settings → Authentication.",
			"AuthDisabled": true,
		})
		return
	}
	s.renderTemplate(w, "login.html", map[string]any{
		"CSRFToken": s.csrfToken("anon"),
	})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.EnablePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	ip := clientIP(r)
	if !s.loginRL.check(ip) {
		logx.Warnf("login rate-limited from %s", ip)
		s.renderTemplate(w, "login.html", map[string]any{
			"Error":     "Too many attempts. Please wait before trying again.",
			"CSRFToken": s.csrfToken("anon"),
		})
		return
	}

	password := r.FormValue("password")
	if err := bcrypt.CompareHashAndPassword(
		[]byte(s.cfg.Auth.PasswordHash), []byte(password),
	); err != nil {
		s.loginRL.recordFailure(ip)
		logx.Warnf("login failed from %s", ip)
		s.renderTemplate(w, "login.html", map[string]any{
			"Error":     "Invalid password",
			"CSRFToken": s.csrfToken("anon"),
		})
		return
	}
	s.loginRL.recordSuccess(ip)
	logx.Infof("login success from %s", ip)

	sessID, err := s.sessions.NewSession(s.cfg.Auth.SessionLifetimeDays)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "monbooru_session",
		Value:    sessID,
		Path:     "/",
		MaxAge:   s.cfg.Auth.SessionLifetimeDays * 86400,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	sessID := sessionFromContext(r.Context())
	s.sessions.DeleteSession(sessID)
	http.SetCookie(w, &http.Cookie{
		Name:   "monbooru_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// renderAuthPasswordOOB writes an out-of-band swap for the password subsection
// so the "currently enabled/disabled" text and form fields reflect the latest
// auth state without requiring a page reload.
func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/auth_password_section.html", map[string]any{
		"AuthEnabled": s.cfg.Auth.EnablePassword,
		"CSRFToken":   s.csrfToken(sessionFromContext(r.Context())),
		"OOB":         true,
	})
}
