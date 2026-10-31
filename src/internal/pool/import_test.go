package pool

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestImportAccountsDryRunDoesNotTouchFile(t *testing.T) {
	file := writeAccounts(t, []map[string]any{{"id": "seed", "access_token": "old", "enabled": true, "priority": 1}})
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取导入前文件: %v", err)
	}
	p := New(Config{File: file}, nil, testLogger())

	result, err := p.ImportAccounts([]*Account{
		{ID: "new-1", AccessToken: "token-new", Enabled: true, Priority: 100},
	}, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run 导入失败: %v", err)
	}
	if result.Total != 1 || result.Added != 1 || result.Updated != 0 || result.Skipped != 0 || result.Invalid != 0 {
		t.Fatalf("dry-run 统计不符: %+v", result)
	}
	if _, ok := p.View("new-1"); ok {
		t.Fatal("dry-run 不应把账号加入内存池")
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取导入后文件: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry-run 改动了账号文件\nbefore=%s\nafter=%s", before, after)
	}
}

func TestImportAccountsOverwritePreservesSchedulingSettings(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	file := writeAccounts(t, []map[string]any{{
		"id": "seed", "name": "旧名字", "user_id": "u-old", "access_token": "old-token",
		"refresh_token": "old-refresh", "enabled": false, "priority": 7, "weight": 42,
		"cooldown_until": future, "consecutive_fails": 4,
		"last_failure_at": time.Now().UnixMilli(), "last_error": "旧错误",
		"custom_field": "old-extra",
	}})
	p := New(Config{File: file}, nil, testLogger())

	incoming := &Account{
		ID: "seed", Name: "新名字", UserID: "u-new", AccessToken: "new-token",
		ExpiresAt: 123456, Enabled: true, Priority: 1, Weight: 99,
	}
	incoming.SetExtra("office_identity", json.RawMessage(`"org-new"`))
	incoming.SetExtra("phone", json.RawMessage(`"13800000000"`))

	result, err := p.ImportAccounts([]*Account{incoming}, ImportOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("覆盖导入失败: %v", err)
	}
	if result.Updated != 1 || result.Added != 0 || result.Skipped != 0 {
		t.Fatalf("覆盖统计不符: %+v", result)
	}
	view, ok := p.View("seed")
	if !ok {
		t.Fatal("覆盖后账号丢失")
	}
	if view.Enabled || view.Priority != 7 || view.Weight != 42 {
		t.Fatalf("人工设置未保留: enabled=%v priority=%d weight=%d", view.Enabled, view.Priority, view.Weight)
	}
	if view.CooldownUntil != 0 || view.ConsecutiveFails != 0 || view.LastError != "" {
		t.Fatalf("旧凭证失败状态未清除: %+v", view)
	}

	accounts := readRawAccounts(t, file)
	seed := accounts[0]
	assertRawString(t, seed, "name", "新名字")
	assertRawString(t, seed, "user_id", "u-new")
	assertRawString(t, seed, "access_token", "new-token")
	assertRawString(t, seed, "office_identity", "org-new")
	assertRawString(t, seed, "phone", "13800000000")
	assertRawString(t, seed, "custom_field", "old-extra")
	var weight int
	_ = json.Unmarshal(seed["weight"], &weight)
	if weight != 42 {
		t.Fatalf("weight = %d, 期望保留 42", weight)
	}
}

func TestImportAccountsDuplicateIDs(t *testing.T) {
	file := writeAccounts(t, nil)
	p := New(Config{File: file}, nil, testLogger())
	first := &Account{ID: "same", AccessToken: "first", Enabled: true, Priority: 100}
	last := &Account{ID: "same", AccessToken: "last", Enabled: true, Priority: 100}

	result, err := p.ImportAccounts([]*Account{first, last}, ImportOptions{})
	if err != nil {
		t.Fatalf("重复导入失败: %v", err)
	}
	if result.Added != 1 || result.Skipped != 1 || result.Updated != 0 {
		t.Fatalf("overwrite=false 统计不符: %+v", result)
	}
	if got := p.TokenFor("same"); got != "first" {
		t.Fatalf("overwrite=false 应保留首条，实际 token=%q", got)
	}

	result, err = p.ImportAccounts([]*Account{first, last}, ImportOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("覆盖重复导入失败: %v", err)
	}
	if result.Added != 0 || result.Updated != 1 || result.Skipped != 1 {
		t.Fatalf("overwrite=true 统计不符: %+v", result)
	}
	if got := p.TokenFor("same"); got != "last" {
		t.Fatalf("overwrite=true 应保留末条，实际 token=%q", got)
	}
}

func TestImportAccountsWithoutOverwriteSkipsExisting(t *testing.T) {
	file := writeAccounts(t, []map[string]any{{"id": "seed", "access_token": "old", "enabled": true}})
	p := New(Config{File: file}, nil, testLogger())
	result, err := p.ImportAccounts([]*Account{{ID: "seed", AccessToken: "new", Enabled: true}}, ImportOptions{})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if result.Skipped != 1 || result.Added != 0 || result.Updated != 0 {
		t.Fatalf("统计不符: %+v", result)
	}
	if got := p.TokenFor("seed"); got != "old" {
		t.Fatalf("未覆盖账号 token 被改变: %q", got)
	}
}

// TestImportAccountsSerializesConcurrentPoolChanges 在导入写事务期间启动其他账号
// 增删改，验证这些写路径会等待导入落盘，且最终内存和磁盘均不丢更新。
func TestImportAccountsSerializesConcurrentPoolChanges(t *testing.T) {
	file := writeAccounts(t, []map[string]any{
		{"id": "update-me", "name": "旧名字", "access_token": "update-token", "enabled": true},
		{"id": "remove-me", "name": "待删除", "access_token": "remove-token", "enabled": true},
	})
	p := New(Config{File: file}, nil, testLogger())

	concurrentName := "并发更新后的名字"
	started := make(chan struct{}, 3)
	errCh := make(chan error, 3)
	result, err := p.importAccounts([]*Account{{
		ID: "imported", AccessToken: "imported-token", Enabled: true,
	}}, ImportOptions{}, importHooks{beforeWrite: func() {
		go func() {
			started <- struct{}{}
			errCh <- p.Add(&Account{ID: "concurrent-add", AccessToken: "concurrent-token", Enabled: true})
		}()
		go func() {
			started <- struct{}{}
			errCh <- p.Update("update-me", AccountPatch{Name: &concurrentName})
		}()
		go func() {
			started <- struct{}{}
			errCh <- p.Remove("remove-me")
		}()
		<-started
		<-started
		<-started
	}})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if result.Added != 1 || result.Updated != 0 || result.Total != 1 {
		t.Fatalf("导入统计不符: %+v", result)
	}
	for _, err := range collectConcurrentErrors(t, errCh, 3) {
		if err != nil {
			t.Fatalf("并发账号写失败: %v", err)
		}
	}

	updated, ok := p.View("update-me")
	if !ok || updated.Name != concurrentName {
		t.Fatalf("并发更新被导入覆盖: %+v, exists=%v", updated, ok)
	}
	if _, ok := p.View("concurrent-add"); !ok {
		t.Fatal("并发新增账号被导入覆盖")
	}
	if _, ok := p.View("remove-me"); ok {
		t.Fatal("已并发删除的账号被导入旧快照带回")
	}
	if got := p.TokenFor("imported"); got != "imported-token" {
		t.Fatalf("导入账号 token = %q", got)
	}

	rawByID := make(map[string]map[string]json.RawMessage)
	for _, raw := range readRawAccounts(t, file) {
		var id string
		if err := json.Unmarshal(raw["id"], &id); err != nil {
			t.Fatalf("解析账号 id 失败: %v", err)
		}
		rawByID[id] = raw
	}
	if len(rawByID) != 3 {
		t.Fatalf("最终账号文件条目数 = %d, 期望 3: %+v", len(rawByID), rawByID)
	}
	assertRawString(t, rawByID["update-me"], "name", concurrentName)
	assertRawString(t, rawByID["concurrent-add"], "access_token", "concurrent-token")
	assertRawString(t, rawByID["imported"], "access_token", "imported-token")
	if _, ok := rawByID["remove-me"]; ok {
		t.Fatal("已删除账号仍写入账号文件")
	}
}

// TestImportAccountsPreservesConcurrentAcquireDuringWrite 在导入写盘期间占用账号，
// 验证 Acquire 不被写事务长时间阻塞，且正常导入不会重置在途计数与租约。
func TestImportAccountsPreservesConcurrentAcquireDuringWrite(t *testing.T) {
	file := writeAccounts(t, []map[string]any{{
		"id": "seed", "access_token": "seed-token", "enabled": true, "priority": 1,
	}})
	p := New(Config{File: file}, nil, testLogger())

	var acquired *Account
	result, err := p.importAccounts([]*Account{{
		ID: "seed", AccessToken: "new-token", Enabled: true,
	}}, ImportOptions{Overwrite: true}, importHooks{beforeWrite: func() {
		acquiredCh := make(chan *Account, 1)
		go func() { acquiredCh <- p.Acquire(nil) }()
		select {
		case acquired = <-acquiredCh:
		case <-time.After(2 * time.Second):
			t.Fatal("Acquire 被导入写事务阻塞")
		}
	}})
	if err != nil {
		t.Fatalf("并发 Acquire 期间导入失败: %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("导入统计不符: %+v", result)
	}
	if acquired == nil || acquired.ID != "seed" {
		t.Fatalf("并发获取账号失败: %+v", acquired)
	}
	if view := accountViewByID(t, p, "seed"); view.InFlight != 1 {
		t.Fatalf("导入重置了并发 Acquire 的在途计数: %+v", view)
	}
	if view := accountViewByID(t, p, "seed"); view.LastUsedAt != acquired.LastUsedAt {
		t.Fatalf("导入回退了并发 Acquire 的 last_used_at: got=%d want=%d", view.LastUsedAt, acquired.LastUsedAt)
	}
	if got := p.TokenFor("seed"); got != "new-token" {
		t.Fatalf("导入后 token = %q, 期望 new-token", got)
	}

	p.Release(acquired)
	if view := accountViewByID(t, p, "seed"); view.InFlight != 0 {
		t.Fatalf("Release 后在途计数 = %d, 期望 0", view.InFlight)
	}
}

// TestImportAccountsPersistFailureKeepsConcurrentMutations 在导入写盘期间并发
// 新增与更新，并强制本次导入写盘失败；最终内存和磁盘只能包含并发变更。
func TestImportAccountsPersistFailureKeepsConcurrentMutations(t *testing.T) {
	file := writeAccounts(t, []map[string]any{{
		"id": "seed", "name": "旧名字", "access_token": "seed-token", "enabled": true,
	}})
	p := New(Config{File: file}, nil, testLogger())

	concurrentName := "并发更新后的名字"
	started := make(chan struct{}, 2)
	errCh := make(chan error, 2)
	_, err := p.importAccounts([]*Account{{
		ID: "imported", AccessToken: "imported-token", Enabled: true,
	}}, ImportOptions{}, importHooks{
		beforeWrite: func() {
			go func() {
				started <- struct{}{}
				errCh <- p.Add(&Account{ID: "concurrent-add", AccessToken: "concurrent-token", Enabled: true})
			}()
			go func() {
				started <- struct{}{}
				errCh <- p.Update("seed", AccountPatch{Name: &concurrentName})
			}()
			<-started
			<-started
			// 并发写已进入等待队列；把导入目标换成目录让本次 rename 失败。
			p.cfg.File = t.TempDir()
		},
		afterWrite: func(error) {
			// 必须在释放 writeMu 前恢复路径，让随后执行的并发写成功落盘。
			p.cfg.File = file
		},
	})
	if err == nil {
		t.Fatal("导入写盘失败应返回错误")
	}
	for _, err := range collectConcurrentErrors(t, errCh, 2) {
		if err != nil {
			t.Fatalf("并发新增/更新失败: %v", err)
		}
	}
	if _, ok := p.View("imported"); ok {
		t.Fatal("导入写盘失败后内存仍残留本次导入")
	}
	if _, ok := p.View("concurrent-add"); !ok {
		t.Fatal("导入写盘失败覆盖了并发新增")
	}
	if view, ok := p.View("seed"); !ok || view.Name != concurrentName {
		t.Fatalf("导入写盘失败覆盖了并发更新: %+v, exists=%v", view, ok)
	}

	rawByID := make(map[string]map[string]json.RawMessage)
	for _, raw := range readRawAccounts(t, file) {
		var id string
		if err := json.Unmarshal(raw["id"], &id); err != nil {
			t.Fatalf("解析账号 id 失败: %v", err)
		}
		rawByID[id] = raw
	}
	if len(rawByID) != 2 {
		t.Fatalf("最终账号文件条目数 = %d, 期望 2: %+v", len(rawByID), rawByID)
	}
	if _, ok := rawByID["imported"]; ok {
		t.Fatal("导入写盘失败后磁盘仍残留本次导入")
	}
	assertRawString(t, rawByID["seed"], "name", concurrentName)
	assertRawString(t, rawByID["seed"], "access_token", "seed-token")
	assertRawString(t, rawByID["concurrent-add"], "access_token", "concurrent-token")
}

// TestImportAccountsPreservesExistingInflightLease 覆盖覆盖导入不重置同 ID 账号的
// 在途计数与租约，后续 Release 仍能正确归还。
func TestImportAccountsPreservesExistingInflightLease(t *testing.T) {
	file := writeAccounts(t, []map[string]any{{
		"id": "seed", "name": "旧名字", "access_token": "old-token", "enabled": true,
	}})
	p := New(Config{File: file}, nil, testLogger())
	acquired := p.Acquire(nil)
	if acquired == nil || acquired.ID != "seed" {
		t.Fatalf("获取在途账号失败: %+v", acquired)
	}
	if view := accountViewByID(t, p, "seed"); view.InFlight != 1 {
		t.Fatalf("导入前在途计数 = %d, 期望 1", view.InFlight)
	}

	result, err := p.ImportAccounts([]*Account{{
		ID: "seed", Name: "新名字", AccessToken: "new-token", Enabled: true,
	}}, ImportOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("覆盖导入失败: %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("覆盖统计不符: %+v", result)
	}
	view := accountViewByID(t, p, "seed")
	if view.InFlight != 1 || view.Name != "新名字" {
		t.Fatalf("导入重置了在途计数或未更新字段: %+v", view)
	}
	if got := p.TokenFor("seed"); got != "new-token" {
		t.Fatalf("覆盖后 token = %q", got)
	}

	p.Release(acquired)
	if view := accountViewByID(t, p, "seed"); view.InFlight != 0 {
		t.Fatalf("Release 后在途计数 = %d, 期望 0", view.InFlight)
	}
}

func TestImportAccountsPersistFailureLeavesMemoryUnchanged(t *testing.T) {
	file := writeAccounts(t, []map[string]any{{"id": "seed", "access_token": "old", "enabled": true}})
	p := New(Config{File: file}, nil, testLogger())
	p.cfg.File = t.TempDir() // rename 到目录必然失败

	_, err := p.ImportAccounts([]*Account{{ID: "new", AccessToken: "token", Enabled: true}}, ImportOptions{})
	if err == nil {
		t.Fatal("写盘失败应返回错误")
	}
	if _, ok := p.View("new"); ok {
		t.Fatal("写盘失败后应恢复旧账号池")
	}
	if _, ok := p.View("seed"); !ok {
		t.Fatal("写盘失败后旧账号应仍在池中")
	}
}

func collectConcurrentErrors(t *testing.T, ch <-chan error, count int) []error {
	t.Helper()
	errs := make([]error, 0, count)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(errs) < count {
		select {
		case err := <-ch:
			errs = append(errs, err)
		case <-timer.C:
			t.Fatalf("等待并发账号写完成超时：已收到 %d/%d", len(errs), count)
		}
	}
	return errs
}

func readRawAccounts(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取账号文件: %v", err)
	}
	var payload struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("解析账号文件: %v", err)
	}
	return payload.Accounts
}

func assertRawString(t *testing.T, raw map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(raw[key], &got); err != nil {
		t.Fatalf("字段 %s 不是字符串: %v", key, err)
	}
	if got != want {
		t.Fatalf("字段 %s = %q, 期望 %q", key, got, want)
	}
}
