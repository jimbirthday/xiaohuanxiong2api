// Package server 实现 §6 的 HTTP 服务：路由、鉴权、健康检查、模型列表、聊天转发。
package server

import (
	"net/http"
	"time"

	"xhx2api/internal/auth"
	"xhx2api/internal/checkin"
	"xhx2api/internal/config"
	"xhx2api/internal/logx"
	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

// Server 持有 HTTP 服务所需的全部共享状态。
type Server struct {
	cfg     *config.Config
	client  *upstream.Client
	pool    *pool.Pool
	catalog *Catalog
	logs    *LogBuffer
	logger  *logx.Logger
	checkin *checkin.Service
	login   *loginManager

	authManager  *auth.Manager
	cookieSecure bool
	loginLimiter *loginLimiter

	startedAt time.Time
	version   string
}

// New 构造 Server。
func New(cfg *config.Config, client *upstream.Client, accountPool *pool.Pool, catalog *Catalog, logger *logx.Logger, ver string) *Server {
	return &Server{
		cfg:          cfg,
		client:       client,
		pool:         accountPool,
		catalog:      catalog,
		logs:         NewLogBuffer(cfg.Log.BufferSizeValue()),
		logger:       logger,
		login:        newLoginManager(),
		loginLimiter: newLoginLimiter(),
		startedAt:    time.Now(),
		version:      ver,
	}
}

// SetCheckin 注入每日签到服务（供 /admin/api/checkin 使用）。
func (s *Server) SetCheckin(svc *checkin.Service) { s.checkin = svc }

// SetAuth 注入管理员认证管理器与 Cookie Secure 配置。
func (s *Server) SetAuth(manager *auth.Manager, cookieSecure bool) {
	s.authManager = manager
	s.cookieSecure = cookieSecure
}

// Handler 返回带路由与中间件的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	// favicon 必须在鉴权之前短路（SPEC §6.1）。
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.Handle("GET /v1/models", s.v1Auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", s.v1Auth(http.HandlerFunc(s.handleChat)))
	mux.HandleFunc("GET /admin", s.handleAdminPage)
	mux.HandleFunc("GET /admin/{$}", s.handleAdminPage)
	mux.HandleFunc("/admin/api/auth/login", s.adminLogin)
	mux.HandleFunc("/admin/api/auth/session", s.adminSession)
	mux.Handle("/admin/api/auth/logout", s.adminAuth(http.HandlerFunc(s.adminLogout)))
	mux.Handle("/admin/api/", s.adminAuth(http.HandlerFunc(s.handleAdminAPI)))
	return s.securityHeaders(s.adminAvailability(mux))
}

// handleHealth 实现 GET /healthz（SPEC §6.1）。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	total, _, available, _ := s.pool.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.version,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"accounts": map[string]any{
			"total":     total,
			"available": available,
		},
		"models": s.catalog.Count(),
	})
}

// handleFavicon 返回 204（在鉴权之前短路）。
func (s *Server) handleFavicon(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// Config 返回当前配置（供 §7.1 使用）。
func (s *Server) Config() *config.Config { return s.cfg }

// Catalog 返回目录（供 §7.1 使用）。
func (s *Server) Catalog() *Catalog { return s.catalog }

// Pool 返回账号池（供 §7.1 使用）。
func (s *Server) Pool() *pool.Pool { return s.pool }

// Logs 返回请求日志缓冲（供 §7.1 使用）。
func (s *Server) Logs() *LogBuffer { return s.logs }

// StartedAt 返回进程启动时间。
func (s *Server) StartedAt() time.Time { return s.startedAt }
