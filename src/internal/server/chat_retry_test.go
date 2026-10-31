package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/config"
)

func assertServerNoInflight(t *testing.T, env *testEnv) {
	t.Helper()
	for _, view := range env.s.pool.Views() {
		if view.InFlight != 0 {
			t.Fatalf("账号 %s 在途计数 = %d, 期望 0", view.ID, view.InFlight)
		}
	}
}

// TestParseRetryAfter 覆盖整数秒、HTTP 日期和非法值。
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"integer", "7", 7 * time.Second},
		{"zero", "0", 0},
		{"http-date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"past-date", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"invalid", "soon", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value, now); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %s, 期望 %s", tc.value, got, tc.want)
			}
		})
	}
}

// Test429RetryAfterCooldownAndSwitch 覆盖 429 Retry-After 与基础冷却取较大值后换号。
func Test429RetryAfterCooldownAndSwitch(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 429001, "message": "too many requests"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "c", "object": "chat.completion", "model": "internal",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{
		{"id": "a1", "access_token": "token-a", "enabled": true, "priority": 1},
		{"id": "a2", "access_token": "token-b", "enabled": true, "priority": 2},
	}, func(cfg *config.Config) {
		cfg.Accounts.Strategy = sp("priority")
		cfg.Accounts.CooldownSeconds = ip(1)
		cfg.Accounts.CooldownMaxSeconds = ip(5)
	})

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	view, ok := env.s.pool.View("a1")
	if !ok {
		t.Fatal("账号 a1 不存在")
	}
	remaining := time.Until(time.UnixMilli(view.CooldownUntil))
	if remaining < 1500*time.Millisecond || remaining > 3*time.Second {
		t.Fatalf("429 后剩余冷却 = %s, 期望约 2s", remaining)
	}
	assertServerNoInflight(t, env)
}

// TestFinal429RetryAfterUsesMinimumRemaining 覆盖最终 429 的 Retry-After 取已尝试账号最小剩余值。
func TestFinal429RetryAfterUsesMinimumRemaining(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		retryAfter := "6"
		if r.Header.Get("Authorization") == "Bearer token-b" {
			retryAfter = "3"
		}
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 429001, "message": "too many requests"})
	}
	env := newTestEnv(t, handler, []map[string]any{
		{"id": "a1", "access_token": "token-a", "enabled": true, "priority": 1},
		{"id": "a2", "access_token": "token-b", "enabled": true, "priority": 2},
	}, func(cfg *config.Config) {
		cfg.Accounts.Strategy = sp("priority")
		cfg.Accounts.MaxAttempts = ip(2)
		cfg.Accounts.CooldownSeconds = ip(1)
		cfg.Accounts.CooldownMaxSeconds = ip(10)
	})

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds < 1 || seconds > 3 {
		t.Fatalf("最终 Retry-After = %q, 期望约 3 秒", resp.Header.Get("Retry-After"))
	}
	assertServerNoInflight(t, env)
}

// Test5xxRecordsFailureAndSwitches 覆盖 5xx 按失败退避冷却并换号。
func Test5xxRecordsFailureAndSwitches(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-a" {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 500001, "message": "boom"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "c", "object": "chat.completion", "model": "internal",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{
		{"id": "a1", "access_token": "token-a", "enabled": true, "priority": 1},
		{"id": "a2", "access_token": "token-b", "enabled": true, "priority": 2},
	}, func(cfg *config.Config) {
		cfg.Accounts.Strategy = sp("priority")
		cfg.Accounts.ServerErrorCooldownSeconds = ip(1)
		cfg.Accounts.CooldownMaxSeconds = ip(10)
	})

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	view, _ := env.s.pool.View("a1")
	if view.ConsecutiveFails != 1 {
		t.Fatalf("连续失败 = %d, 期望 1", view.ConsecutiveFails)
	}
	remaining := time.Until(time.UnixMilli(view.CooldownUntil))
	if remaining <= 0 || remaining > 1500*time.Millisecond {
		t.Fatalf("5xx 退避剩余 = %s, 期望约 1s", remaining)
	}
	assertServerNoInflight(t, env)
}

// Test401RefreshesSameAccountOnlyOnceThenSwitches 覆盖同号强制刷新只重试一次，再 401 换号。
func Test401RefreshesSameAccountOnlyOnceThenSwitches(t *testing.T) {
	var chatCount, refreshCount int64
	var mu sync.Mutex
	var tokens []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refresh":
			atomic.AddInt64(&refreshCount, 1)
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]string{"access_token": "refreshed-token", "refresh_token": "rt-new"},
			})
		case "/chat/completions":
			atomic.AddInt64(&chatCount, 1)
			auth := r.Header.Get("Authorization")
			mu.Lock()
			tokens = append(tokens, auth)
			mu.Unlock()
			if auth == "Bearer token-b" {
				writeJSON(w, http.StatusOK, map[string]any{
					"id": "c", "object": "chat.completion", "model": "internal",
					"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
				})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200003, "message": "authorization_verify_error"})
		default:
			http.NotFound(w, r)
		}
	}
	env := newTestEnv(t, handler, []map[string]any{
		{"id": "a1", "access_token": "token-a", "refresh_token": "rt-old", "enabled": true, "priority": 1},
		{"id": "a2", "access_token": "token-b", "enabled": true, "priority": 2},
	}, func(cfg *config.Config) {
		cfg.Accounts.Strategy = sp("priority")
		cfg.Accounts.MaxAttempts = ip(3)
		cfg.Accounts.RefreshOn401 = bp(true)
	})

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	if got := atomic.LoadInt64(&chatCount); got != 3 {
		t.Fatalf("chat 调用次数 = %d, 期望 3（a1 首次 + a1 刷新重试 + a2）", got)
	}
	if got := atomic.LoadInt64(&refreshCount); got != 1 {
		t.Fatalf("refresh 次数 = %d, 期望 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 3 || tokens[0] != "Bearer token-a" || tokens[1] != "Bearer refreshed-token" || tokens[2] != "Bearer token-b" {
		t.Fatalf("实际请求 token 顺序 = %v", tokens)
	}
	assertServerNoInflight(t, env)
}

// TestOther4xxNoRetryNoCooldownAndRelease 覆盖其它 4xx 原样透传且不换号、不冷却。
func TestOther4xxNoRetryNoCooldownAndRelease(t *testing.T) {
	var calls int64
	handler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 400001, "message": "bad request"})
	}
	env := newTestEnv(t, handler, []map[string]any{
		{"id": "a1", "access_token": "token-a", "enabled": true, "priority": 1},
		{"id": "a2", "access_token": "token-b", "enabled": true, "priority": 2},
	}, func(cfg *config.Config) { cfg.Accounts.Strategy = sp("priority") })

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("其它 4xx 请求次数 = %d, 期望 1（不换号）", got)
	}
	view, _ := env.s.pool.View("a1")
	if view.ConsecutiveFails != 0 || view.CooldownUntil != 0 {
		t.Fatalf("其它 4xx 不应记录失败/冷却: %+v", view)
	}
	assertServerNoInflight(t, env)
}
