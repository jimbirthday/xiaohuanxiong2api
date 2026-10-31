package pool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/upstream"
)

// makeJWT 造一个只含 exp 的假 JWT（不验签，只解 payload）。
func makeJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return header + "." + payload + ".sig"
}

// newRefreshStub 起一个只处理 /refresh 的桩上游，返回计数与客户端。
func newRefreshStub(t *testing.T, accessToken string, delay time.Duration) (*upstream.Client, *int64) {
	t.Helper()
	var count int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&count, 1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]string{
				"access_token":  accessToken,
				"refresh_token": "rt-new",
			},
		})
	}))
	t.Cleanup(server.Close)
	client := upstream.NewClient(upstream.Options{
		AuthBase:       server.URL,
		LLMBase:        server.URL,
		PointsBase:     server.URL,
		RefreshTimeout: 5 * time.Second,
	})
	return client, &count
}

func writeAccounts(t *testing.T, accounts []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	payload := map[string]any{"accounts": accounts}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化账号失败: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入账号文件失败: %v", err)
	}
	return path
}

func testLogger() *logx.Logger { return logx.New(io.Discard, "", logx.LevelInfo) }

func newSchedulingPool(t *testing.T, strategy string, accounts []map[string]any, mutate func(*Config)) *Pool {
	t.Helper()
	cfg := Config{
		File:                       writeAccounts(t, accounts),
		Strategy:                   strategy,
		RefreshMarginSeconds:       300,
		CooldownSeconds:            600,
		MaxInflightPerAccount:      3,
		ServerErrorCooldownSeconds: 30,
		FailureDecaySeconds:        900,
		CooldownMaxSeconds:         3600,
		MaxAttempts:                3,
		RefreshOn401:               true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client := upstream.NewClient(upstream.Options{})
	return New(cfg, client, testLogger())
}

func accountViewByID(t *testing.T, p *Pool, id string) AccountView {
	t.Helper()
	view, ok := p.View(id)
	if !ok {
		t.Fatalf("账号 %s 不存在", id)
	}
	return view
}

func assertNoInflight(t *testing.T, p *Pool) {
	t.Helper()
	for _, view := range p.Views() {
		if view.InFlight != 0 {
			t.Fatalf("账号 %s 在途计数 = %d, 期望 0", view.ID, view.InFlight)
		}
	}
}

// TestExpiringTriggersRefresh 覆盖「临期触发刷新」。
func TestExpiringTriggersRefresh(t *testing.T) {
	newToken := makeJWT(time.Now().Add(24 * time.Hour).Unix())
	client, count := newRefreshStub(t, newToken, 0)
	file := writeAccounts(t, []map[string]any{{
		"id":            "a1",
		"name":          "账号1",
		"access_token":  makeJWT(time.Now().Add(24 * time.Hour).Unix()),
		"refresh_token": "rt-old",
		"expires_at":    time.Now().Add(100 * time.Second).UnixMilli(), // 小于 300s 边距
		"enabled":       true,
		"priority":      1,
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	token, err := p.Token(context.Background(), "a1", false)
	if err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	if token != newToken {
		t.Fatalf("临期应刷新并返回新 token")
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("刷新次数 = %d, 期望 1", got)
	}
	// 回写后 refresh_token 应轮换为新值。
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "rt-new") {
		t.Fatalf("账号文件应回写新 refresh_token，实际: %s", data)
	}
}

// TestConcurrentRefreshSingleFlight 覆盖「10 个并发请求只发出 1 次 refresh」。
func TestConcurrentRefreshSingleFlight(t *testing.T) {
	newToken := makeJWT(time.Now().Add(24 * time.Hour).Unix())
	client, count := newRefreshStub(t, newToken, 50*time.Millisecond)
	file := writeAccounts(t, []map[string]any{{
		"id":            "a1",
		"access_token":  "old-token",
		"refresh_token": "rt-old",
		"expires_at":    time.Now().Add(24 * time.Hour).UnixMilli(),
		"enabled":       true,
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Token(context.Background(), "a1", true); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发刷新失败: %v", err)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("并发刷新真实请求数 = %d, 期望 1", got)
	}
}

// TestPersistPreservesUnknownFields 覆盖「回写保留未知字段」。
func TestPersistPreservesUnknownFields(t *testing.T) {
	newToken := makeJWT(time.Now().Add(24 * time.Hour).Unix())
	client, _ := newRefreshStub(t, newToken, 0)
	file := writeAccounts(t, []map[string]any{{
		"id":            "a1",
		"access_token":  "old",
		"refresh_token": "rt-old",
		"expires_at":    time.Now().Add(100 * time.Second).UnixMilli(),
		"enabled":       true,
		"custom_field":  "keep-me",
		"nested":        map[string]any{"x": 1},
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())
	if _, err := p.Token(context.Background(), "a1", false); err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取账号文件失败: %v", err)
	}
	var parsed struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("解析回写文件失败: %v", err)
	}
	if len(parsed.Accounts) != 1 {
		t.Fatalf("账号数 = %d", len(parsed.Accounts))
	}
	if _, ok := parsed.Accounts[0]["custom_field"]; !ok {
		t.Fatalf("未知字段 custom_field 丢失: %s", data)
	}
	if _, ok := parsed.Accounts[0]["nested"]; !ok {
		t.Fatalf("未知字段 nested 丢失: %s", data)
	}
}

// TestPickPrioritizesHealthyTier 覆盖连续失败账号即使 priority 更高也先让位给健康账号。
func TestPickPrioritizesHealthyTier(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "hot", "access_token": "tok-hot", "enabled": true, "priority": 1},
		{"id": "healthy", "access_token": "tok-healthy", "enabled": true, "priority": 100},
	}, nil)

	p.mu.Lock()
	hot := p.findLocked("hot")
	hot.ConsecutiveFails = 2
	hot.LastFailureAt = time.Now().UnixMilli()
	p.mu.Unlock()

	got := p.Acquire(nil)
	if got == nil || got.ID != "healthy" {
		t.Fatalf("失败层账号不应抢占健康账号，实际选中 %+v", got)
	}
	p.Release(got)

	p.RecordSuccess("hot")
	got = p.Acquire(nil)
	if got == nil || got.ID != "hot" {
		t.Fatalf("恢复健康后应按 priority 选中 hot，实际 %+v", got)
	}
	p.Release(got)
}

// TestPriorityOrderUsesLRUAndWeight 覆盖 priority 模式的 last_used_at 与 weight 次序。
func TestPriorityOrderUsesLRUAndWeight(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "heavy", "access_token": "tok-heavy", "enabled": true, "priority": 1, "weight": 300},
		{"id": "light", "access_token": "tok-light", "enabled": true, "priority": 1, "weight": 100},
	}, nil)

	p.mu.Lock()
	p.findLocked("heavy").LastUsedAt = 0
	p.findLocked("light").LastUsedAt = 0
	p.mu.Unlock()
	got := p.Acquire(nil)
	if got == nil || got.ID != "heavy" {
		t.Fatalf("同 priority/last_used 时应选 weight 更大的账号，实际 %+v", got)
	}
	p.Release(got)

	p.mu.Lock()
	p.findLocked("heavy").LastUsedAt = time.Now().UnixMilli()
	p.findLocked("light").LastUsedAt = time.Now().Add(-time.Minute).UnixMilli()
	p.mu.Unlock()
	got = p.Acquire(nil)
	if got == nil || got.ID != "light" {
		t.Fatalf("priority 相同时应优先 LRU 更旧的账号，实际 %+v", got)
	}
	p.Release(got)
}

// TestRoundRobinWeightedLRU 覆盖加权轮转比例，以及同分平局时选择更久未用账号。
func TestRoundRobinWeightedLRU(t *testing.T) {
	lruPool := newSchedulingPool(t, "round_robin", []map[string]any{
		{"id": "a", "access_token": "tok-a", "enabled": true, "weight": 100},
		{"id": "b", "access_token": "tok-b", "enabled": true, "weight": 100},
	}, nil)

	lruPool.mu.Lock()
	lruPool.findLocked("a").LastUsedAt = time.Now().UnixMilli()
	lruPool.findLocked("b").LastUsedAt = time.Now().Add(-time.Hour).UnixMilli()
	lruPool.mu.Unlock()
	got := lruPool.Acquire(nil)
	if got == nil || got.ID != "b" {
		t.Fatalf("同权重当前值时 LRU 应选更旧的 b，实际 %+v", got)
	}
	lruPool.Release(got)

	p := newSchedulingPool(t, "round_robin", []map[string]any{
		{"id": "a", "access_token": "tok-a", "enabled": true, "weight": 300},
		{"id": "b", "access_token": "tok-b", "enabled": true, "weight": 100},
	}, nil)

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		account := p.Acquire(nil)
		if account == nil {
			t.Fatal("加权轮转不应无账号可选")
		}
		counts[account.ID]++
		p.Release(account)
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("weight=300:100 的轮转比例 = %v, 期望 a=30 b=10", counts)
	}
}

// TestAcquireHonorsInflightLimitAndRelease 覆盖在途上限、幂等归还与 View 在途字段。
func TestAcquireHonorsInflightLimitAndRelease(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "a", "access_token": "tok", "enabled": true, "priority": 1},
	}, func(cfg *Config) { cfg.MaxInflightPerAccount = 1 })

	first := p.Acquire(nil)
	if first == nil {
		t.Fatal("首次 Acquire 应成功")
	}
	if got := accountViewByID(t, p, "a").InFlight; got != 1 {
		t.Fatalf("在途计数 = %d, 期望 1", got)
	}
	if second := p.Acquire(nil); second != nil {
		t.Fatalf("在途满时不应再选中账号，实际 %+v", second)
	}
	p.Release(first)
	p.Release(first) // 重复 Release 必须幂等
	if got := accountViewByID(t, p, "a").InFlight; got != 0 {
		t.Fatalf("Release 后在途计数 = %d, 期望 0", got)
	}
	third := p.Acquire(nil)
	if third == nil {
		t.Fatal("Release 后应重新可 Acquire")
	}
	p.Release(third)
	assertNoInflight(t, p)
}

// TestReleaseLeaseIsPerAcquire 覆盖多个在途请求时重复 Release 只归还对应租约。
func TestReleaseLeaseIsPerAcquire(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "a", "access_token": "tok", "enabled": true, "priority": 1},
	}, func(cfg *Config) { cfg.MaxInflightPerAccount = 2 })

	first := p.Acquire(nil)
	second := p.Acquire(nil)
	if first == nil || second == nil {
		t.Fatalf("两次 Acquire 都应成功: first=%v second=%v", first, second)
	}
	if got := accountViewByID(t, p, "a").InFlight; got != 2 {
		t.Fatalf("在途计数 = %d, 期望 2", got)
	}
	p.Release(first)
	p.Release(first) // 旧租约重复归还不能清掉 second 的在途名额
	if got := accountViewByID(t, p, "a").InFlight; got != 1 {
		t.Fatalf("重复 Release 后在途计数 = %d, 期望 1", got)
	}
	p.Release(second)
	assertNoInflight(t, p)
}

// TestReleasePickSnapshotIsNoop 覆盖只读 Pick 快照不持有租约，Release 不应影响真实在途。
func TestReleasePickSnapshotIsNoop(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "a", "access_token": "tok", "enabled": true, "priority": 1},
	}, func(cfg *Config) { cfg.MaxInflightPerAccount = 2 })

	held := p.Acquire(nil)
	picked := p.Pick(nil)
	if held == nil || picked == nil {
		t.Fatal("Acquire/Pick 应返回账号")
	}
	p.Release(picked)
	if got := accountViewByID(t, p, "a").InFlight; got != 1 {
		t.Fatalf("Release(Pick 快照) 误减在途: %d", got)
	}
	p.Release(held)
	assertNoInflight(t, p)
}

// TestRecordFailureExponentialBackoffAndSuccessReset 覆盖首次退避、指数翻倍、封顶与成功清零。
func TestRecordFailureExponentialBackoffAndSuccessReset(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "a", "access_token": "tok", "enabled": true, "priority": 1},
	}, func(cfg *Config) {
		cfg.ServerErrorCooldownSeconds = 10
		cfg.FailureDecaySeconds = 300
		cfg.CooldownMaxSeconds = 60
	})

	wants := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
	for i, want := range wants {
		if got := p.RecordFailure("a"); got != want {
			t.Fatalf("第 %d 次失败退避 = %s, 期望 %s", i+1, got, want)
		}
	}
	view := accountViewByID(t, p, "a")
	if view.ConsecutiveFails != 4 {
		t.Fatalf("连续失败 = %d, 期望 4", view.ConsecutiveFails)
	}
	if view.CooldownUntil <= time.Now().UnixMilli() {
		t.Fatal("连续失败后账号应处于冷却中")
	}

	p.RecordSuccess("a")
	view = accountViewByID(t, p, "a")
	if view.ConsecutiveFails != 0 {
		t.Fatalf("成功后连续失败 = %d, 期望 0", view.ConsecutiveFails)
	}
	if got := p.RecordFailure("a"); got != 10*time.Second {
		t.Fatalf("成功清零后首次失败退避 = %s, 期望 10s", got)
	}
}

// TestCooldownForCapAndMinRemaining 覆盖显式冷却封顶与最小剩余时长。
func TestCooldownForCapAndMinRemaining(t *testing.T) {
	p := newSchedulingPool(t, "priority", []map[string]any{
		{"id": "a", "access_token": "tok-a", "enabled": true},
		{"id": "b", "access_token": "tok-b", "enabled": true},
	}, func(cfg *Config) { cfg.CooldownMaxSeconds = 5 })

	p.CooldownFor("a", 30*time.Second)
	p.CooldownFor("b", 2*time.Second)
	remaining := p.MinCooldownRemaining([]string{"a", "b"})
	if remaining < 1500*time.Millisecond || remaining > 2500*time.Millisecond {
		t.Fatalf("最小剩余冷却 = %s, 期望约 2s", remaining)
	}
	for _, id := range []string{"a", "b"} {
		view := accountViewByID(t, p, id)
		left := time.Until(time.UnixMilli(view.CooldownUntil))
		if left > 5500*time.Millisecond {
			t.Fatalf("账号 %s 冷却 %s 超过封顶 5s", id, left)
		}
	}
}
