package checkin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

type fixedAccounts struct {
	views []pool.AccountView
}

func (f fixedAccounts) Views() []pool.AccountView { return f.views }

func (f fixedAccounts) Token(context.Context, string, bool) (string, error) { return "token", nil }

func newSchedulerService(t *testing.T, accounts Accounts, handler http.Handler, cfg Config) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := upstream.NewClient(upstream.Options{
		AuthBase:       server.URL,
		PointsBase:     server.URL,
		Origin:         server.URL,
		RefreshTimeout: 2 * time.Second,
		BillsTimeout:   2 * time.Second,
	})
	if cfg.Time == "" {
		cfg.Time = "00:01"
	}
	if cfg.MaxAttemptsPerDay == 0 {
		cfg.MaxAttemptsPerDay = 1
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	svc := New(cfg, client, accounts, logx.New(io.Discard, "", logx.LevelError))
	svc.spacing = 0
	return svc
}

func accountViews(ids ...string) []pool.AccountView {
	views := make([]pool.AccountView, 0, len(ids))
	for _, id := range ids {
		views = append(views, pool.AccountView{ID: id, Enabled: true})
	}
	return views
}

func successfulSetting(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0, "message": "ok",
		"data": map[string]any{"point_grant_popups": []any{}, "point_grant_toast": nil},
	})
}

func TestAccountJitterDeterministic(t *testing.T) {
	first := accountJitter("account-a", "2026-10-31", 60*time.Second)
	second := accountJitter("account-a", "2026-10-31", 60*time.Second)
	if first != second {
		t.Fatalf("同一账号同一天抖动不一致: %v != %v", first, second)
	}
	if first < 0 || first >= 60*time.Second {
		t.Fatalf("抖动越界: %v", first)
	}
	if got := accountJitter("account-a", "2026-10-31", 0); got != 0 {
		t.Fatalf("零上限应返回 0，实际 %v", got)
	}
}

func TestNextWakeDelayCapsSegment(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local)
	if got := nextWakeDelay(now, now.Add(time.Hour), 30*time.Second); got != 30*time.Second {
		t.Fatalf("长等待未分段: %v", got)
	}
	if got := nextWakeDelay(now, now.Add(5*time.Second), 30*time.Second); got != 5*time.Second {
		t.Fatalf("短等待被改动: %v", got)
	}
	if got := nextWakeDelay(now, now.Add(-time.Second), 30*time.Second); got != 0 {
		t.Fatalf("已到点应立即唤醒: %v", got)
	}
}

func TestWaitUntilRechecksWallClock(t *testing.T) {
	svc := newSchedulerService(t, fixedAccounts{views: accountViews("a1")}, http.HandlerFunc(successfulSetting), Config{})
	svc.wakeSegment = 5 * time.Millisecond
	start := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local)
	var calls atomic.Int32
	svc.SetClock(func() time.Time {
		if calls.Add(1) == 1 {
			return start
		}
		return start.Add(2 * time.Hour)
	})

	started := time.Now()
	if !svc.waitUntil(context.Background(), start.Add(time.Hour)) {
		t.Fatal("墙钟跳变后应视为到点")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("墙钟跳变后仍等待过久: %v", elapsed)
	}
}

func TestCheckAllSingleFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		successfulSetting(w, r)
	})
	svc := newSchedulerService(t, fixedAccounts{views: accountViews("a1")}, handler, Config{VerifyBills: false})

	firstDone := make(chan struct{})
	go func() {
		svc.CheckAll(context.Background())
		close(firstDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("首批签到未启动")
	}

	started := time.Now()
	svc.CheckAll(context.Background())
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("重叠触发未被单飞跳过: %v", elapsed)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("首批签到未结束")
	}
}

func TestCheckAllUsesWorkerPool(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
		successfulSetting(w, r)
	})
	svc := newSchedulerService(t, fixedAccounts{views: accountViews("a1", "a2", "a3", "a4", "a5", "a6")},
		handler, Config{VerifyBills: false, Concurrency: 2})

	svc.CheckAll(context.Background())
	if got := peak.Load(); got != 2 {
		t.Fatalf("并发峰值 = %d, 期望 2", got)
	}
	for _, view := range svc.Views() {
		if !view.Today.Success {
			t.Fatalf("账号 %s 未成功: %+v", view.ID, view.Today)
		}
	}
}

func TestStopCancelsSchedulerWaitAndJoins(t *testing.T) {
	svc := newSchedulerService(t, fixedAccounts{views: accountViews("a1")}, http.HandlerFunc(successfulSetting),
		Config{Time: "23:59", OnStart: false})
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local) })
	svc.Start()
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		svc.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop 未能取消等待并 join")
	}
}
