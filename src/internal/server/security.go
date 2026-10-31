package server

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"xhx2api/internal/auth"
)

const (
	adminCookieName        = "xhx2api_admin"
	adminCSRFHeader        = "X-CSRF-Token"
	adminPageCacheControl  = "no-store"
	adminKeyMissingMessage = "请先在 config.json 里设置 admin.api_key，再使用账号管理功能"
	contentSecurityPolicy  = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	loginFailureWindow     = 15 * time.Minute
	loginFailureLimit      = 5
	loginLockDuration      = 15 * time.Minute
)

type adminAuthMode string

const (
	adminAuthSession adminAuthMode = "session"
	adminAuthAPIKey  adminAuthMode = "api_key"
)

type adminPrincipal struct {
	mode    adminAuthMode
	session auth.Session
}

type adminPrincipalContextKey struct{}

// securityHeaders 为所有响应加固定安全头，并覆盖管理页面/接口的缓存策略。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		if isAdminPath(r.URL.Path) {
			header.Set("Cache-Control", adminPageCacheControl)
			// 面板处理器会显式写 no-cache，包装 WriteHeader 以保证最终值仍是 no-store。
			next.ServeHTTP(&adminCacheWriter{ResponseWriter: w}, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminAvailability 在管理面板禁用时让全部 /admin* 路径直接返回 404。
func (s *Server) adminAvailability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminPath(r.URL.Path) && !s.cfg.Admin.AdminEnabled() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAdminPath(path string) bool {
	return strings.HasPrefix(path, "/admin")
}

type adminCacheWriter struct {
	http.ResponseWriter
}

func (w *adminCacheWriter) WriteHeader(statusCode int) {
	w.Header().Set("Cache-Control", adminPageCacheControl)
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *adminCacheWriter) Write(payload []byte) (int, error) {
	w.Header().Set("Cache-Control", adminPageCacheControl)
	return w.ResponseWriter.Write(payload)
}

// v1Auth 只接受配置的 API Key 或认证存储中的 API Key，绝不匿名放行。
func (s *Server) v1Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := bearerToken(r)
		allowed := false
		if configured := s.cfg.APIKeyValue(); configured != "" {
			allowed = secureEqual(provided, configured)
		}
		if !allowed && s.authManager != nil {
			allowed = s.authManager.VerifyAPIKey(provided)
		}
		if !allowed {
			writeError(w, http.StatusUnauthorized, apiError{
				Message: "API key 无效",
				Type:    typeAuthentication,
				Code:    "invalid_api_key",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminAuth 接受管理员会话或旧 admin.api_key，并对会话写请求强制 CSRF。
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Admin.AdminEnabled() {
			http.NotFound(w, r)
			return
		}

		principal, ok := s.authenticateAdmin(r)
		if !ok {
			if s.authManager == nil && s.cfg.Admin.AdminAPIKey() == "" {
				writeError(w, http.StatusForbidden, apiError{
					Message: adminKeyMissingMessage,
					Type:    typeAuthentication,
					Code:    "admin_key_required",
				})
				return
			}
			writeError(w, http.StatusUnauthorized, apiError{
				Message: "管理密钥无效",
				Type:    typeAuthentication,
				Code:    "invalid_admin_key",
			})
			return
		}

		if principal.mode == adminAuthSession && isAdminWriteMethod(r.Method) {
			csrf := r.Header.Get(adminCSRFHeader)
			if csrf == "" || !secureEqual(csrf, principal.session.CSRF) {
				writeError(w, http.StatusForbidden, apiError{
					Message: "CSRF 令牌缺失或无效",
					Type:    typeAuthentication,
					Code:    "csrf_invalid",
				})
				return
			}
		}

		ctx := context.WithValue(r.Context(), adminPrincipalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authenticateAdmin(r *http.Request) (adminPrincipal, bool) {
	configured := s.cfg.Admin.AdminAPIKey()
	if configured != "" {
		if provided := bearerToken(r); provided != "" && secureEqual(provided, configured) {
			return adminPrincipal{mode: adminAuthAPIKey}, true
		}
	}

	if s.authManager != nil {
		if cookie, err := r.Cookie(adminCookieName); err == nil {
			if session, ok := s.authManager.ValidateSession(cookie.Value); ok {
				return adminPrincipal{mode: adminAuthSession, session: session}, true
			}
		}
	}

	if configured == "" {
		return adminPrincipal{}, false
	}
	if r.Method == http.MethodGet {
		if provided := r.URL.Query().Get("key"); provided != "" && secureEqual(provided, configured) {
			return adminPrincipal{mode: adminAuthAPIKey}, true
		}
	}
	return adminPrincipal{}, false
}

func adminPrincipalFromRequest(r *http.Request) (adminPrincipal, bool) {
	principal, ok := r.Context().Value(adminPrincipalContextKey{}).(adminPrincipal)
	return principal, ok
}

func isAdminWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func bearerToken(r *http.Request) string {
	authorization := r.Header.Get("Authorization")
	if strings.HasPrefix(authorization, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	}
	return ""
}

func secureEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

type adminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// adminLogin 是公开的管理员密码登录入口；失败按客户端 IP 做简单限速。
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.cfg.Admin.AdminEnabled() {
		http.NotFound(w, r)
		return
	}
	if s.authManager == nil {
		writeError(w, http.StatusServiceUnavailable, apiError{
			Message: "管理员认证未启用",
			Type:    typeAuthentication,
			Code:    "auth_unavailable",
		})
		return
	}

	ip := clientIP(r)
	if locked, retryAfter := s.loginLimiter.check(ip, time.Now()); locked {
		writeLoginRateLimited(w, retryAfter)
		return
	}

	var request adminLoginRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请求体不是合法 JSON",
			Type:    typeInvalidRequest,
			Code:    "invalid_json",
		})
		return
	}
	username := strings.TrimSpace(request.Username)
	token, csrf, expiresAt, err := s.authManager.Login(username, request.Password)
	if err != nil {
		if locked, retryAfter := s.loginLimiter.recordFailure(ip, time.Now()); locked {
			writeLoginRateLimited(w, retryAfter)
			return
		}
		writeError(w, http.StatusUnauthorized, apiError{
			Message: "用户名或密码错误",
			Type:    typeAuthentication,
			Code:    "invalid_credentials",
		})
		return
	}

	s.loginLimiter.reset(ip)
	s.setSessionCookie(w, r, token, expiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      username,
		"csrf_token":    csrf,
		"expires_at":    expiresAt,
	})
}

// adminSession 是公开的会话探测入口，只根据现有凭据返回登录状态。
func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	principal, ok := s.authenticateAdmin(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, apiError{
			Message: "未登录",
			Type:    typeAuthentication,
			Code:    "authentication_required",
		})
		return
	}

	if principal.mode == adminAuthAPIKey {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"username":      "admin",
			"csrf_token":    "",
			"expires_at":    nil,
			"auth_mode":     adminAuthAPIKey,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      principal.session.Username,
		"csrf_token":    principal.session.CSRF,
		"expires_at":    principal.session.ExpiresAt,
		"auth_mode":     adminAuthSession,
	})
}

// adminLogout 只注销有效会话；CSRF 已由 adminAuth 在进入处理器前校验。
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	principal, ok := adminPrincipalFromRequest(r)
	if !ok || principal.mode != adminAuthSession || s.authManager == nil {
		writeError(w, http.StatusUnauthorized, apiError{
			Message: "需要有效管理员会话",
			Type:    typeAuthentication,
			Code:    "session_required",
		})
		return
	}
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, apiError{
			Message: "需要有效管理员会话",
			Type:    typeAuthentication,
			Code:    "session_required",
		})
		return
	}

	s.authManager.Logout(cookie.Value)
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/admin",
		Expires:  expiresAt,
		MaxAge:   int(s.cfg.Admin.SessionTTL() / time.Second),
		HttpOnly: true,
		Secure:   s.cookieSecure || requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure || requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	forwardedProto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(forwardedProto, "https")
}

func clientIP(r *http.Request) string {
	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil && host != "" {
		return host
	}
	if remoteAddr == "" {
		return "unknown"
	}
	return remoteAddr
}

type loginAttemptState struct {
	failures    []time.Time
	lockedUntil time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttemptState
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttemptState)}
}

func (l *loginLimiter) check(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)

	state := l.attempts[ip]
	if now.Before(state.lockedUntil) {
		return true, state.lockedUntil.Sub(now)
	}
	return false, 0
}

func (l *loginLimiter) recordFailure(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)

	state := l.attempts[ip]
	if now.Before(state.lockedUntil) {
		return true, state.lockedUntil.Sub(now)
	}
	state.failures = append(state.failures, now)
	if len(state.failures) >= loginFailureLimit {
		state.failures = nil
		state.lockedUntil = now.Add(loginLockDuration)
		l.attempts[ip] = state
		return true, loginLockDuration
	}
	l.attempts[ip] = state
	return false, 0
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.attempts, ip)
	l.mu.Unlock()
}

func (l *loginLimiter) pruneLocked(now time.Time) {
	cutoff := now.Add(-loginFailureWindow)
	for ip, state := range l.attempts {
		if !state.lockedUntil.IsZero() {
			if now.Before(state.lockedUntil) {
				continue
			}
			state.lockedUntil = time.Time{}
			state.failures = nil
		}

		kept := state.failures[:0]
		for _, failedAt := range state.failures {
			if failedAt.After(cutoff) {
				kept = append(kept, failedAt)
			}
		}
		state.failures = kept
		if len(state.failures) == 0 {
			delete(l.attempts, ip)
			continue
		}
		l.attempts[ip] = state
	}
}

func writeLoginRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, apiError{
		Message: "登录失败次数过多，请稍后再试",
		Type:    typeRateLimit,
		Code:    "login_rate_limited",
	})
}
