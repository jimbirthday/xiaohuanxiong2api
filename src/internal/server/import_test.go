package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"xhx2api/internal/config"
)

func TestAdminImportJSONAndZIP(t *testing.T) {
	const key = "admin-import-key"
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		[]map[string]any{{"id": "seed", "access_token": "seed-token", "enabled": true, "priority": 1}},
		func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	token := testJWTUser(time.Now().Add(time.Hour).Unix(), "jwt-user")
	browser := marshalTestJSON(t, map[string]any{
		"phone":       "13800000000",
		"cookies_raw": "xhx_token=" + token + "; user_id=cookie-user; user_name=导入用户; office_identity=org-1",
		"jwt":         []string{token},
	})
	standard := marshalTestJSON(t, map[string]any{"accounts": []map[string]any{{
		"id": "standard-user", "name": "标准账号", "access_token": "standard-token",
		"enabled": true, "priority": 9, "unknown_keep": "yes",
	}}})
	archive := makeTestZIP(t, map[string][]byte{"nested/standard.json": standard})

	resp, data := doAdminImport(t, env, key, map[string]string{
		"overwrite": "false", "dry_run": "false",
	}, []importTestFile{
		{Field: "files", Name: "browser.json", Data: browser},
		{Field: "files", Name: "batch.zip", Data: archive},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导入状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var result importResponseForTest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("解析导入响应: %v", err)
	}
	if result.Total != 2 || result.Added != 2 || result.Updated != 0 || result.Skipped != 0 || result.Invalid != 0 {
		t.Fatalf("导入统计不符: %+v", result)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), "standard-token") {
		t.Fatalf("响应泄露完整 token: %s", data)
	}
	if _, ok := env.s.pool.View("cookie-user"); !ok {
		t.Fatal("浏览器导出账号未导入")
	}
	if _, ok := env.s.pool.View("standard-user"); !ok {
		t.Fatal("ZIP 内标准账号未导入")
	}
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	standardRaw := findAccount(accounts, "standard-user")
	if standardRaw == nil || string(standardRaw["unknown_keep"]) != `"yes"` {
		t.Fatalf("标准账号未知字段未保留: %+v", standardRaw)
	}
}

func TestAdminImportDryRunLeavesFileUnchanged(t *testing.T) {
	const key = "admin-import-key"
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		[]map[string]any{{"id": "seed", "access_token": "seed-token", "enabled": true}},
		func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })
	before, err := os.ReadFile(env.s.cfg.Accounts.AccountsFile())
	if err != nil {
		t.Fatalf("读取导入前文件: %v", err)
	}
	payload := marshalTestJSON(t, map[string]any{"accounts": []map[string]any{{
		"id": "dry-user", "access_token": "dry-token",
	}}})

	resp, data := doAdminImport(t, env, key, map[string]string{"dry_run": "true"}, []importTestFile{
		{Field: "file", Name: "dry.json", Data: payload},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dry-run 状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var result importResponseForTest
	_ = json.Unmarshal(data, &result)
	if !result.DryRun || result.Added != 1 {
		t.Fatalf("dry-run 响应不符: %+v", result)
	}
	if _, ok := env.s.pool.View("dry-user"); ok {
		t.Fatal("dry-run 不应把账号加入池")
	}
	after, err := os.ReadFile(env.s.cfg.Accounts.AccountsFile())
	if err != nil {
		t.Fatalf("读取导入后文件: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("dry-run 改动了账号文件")
	}
}

func TestAdminImportOverwritePreservesOperatorSettings(t *testing.T) {
	const key = "admin-import-key"
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		[]map[string]any{{
			"id": "seed", "access_token": "old-token", "enabled": false, "priority": 7,
			"weight": 30, "custom_field": "keep", "cooldown_until": time.Now().Add(time.Hour).UnixMilli(),
		}},
		func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })
	payload := marshalTestJSON(t, map[string]any{"accounts": []map[string]any{{
		"id": "seed", "name": "新名字", "access_token": "new-token",
		"enabled": true, "priority": 1, "weight": 99, "phone": "13900000000",
	}}})

	resp, data := doAdminImport(t, env, key, map[string]string{"overwrite": "true"}, []importTestFile{
		{Field: "files", Name: "overwrite.json", Data: payload},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("覆盖导入状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var result importResponseForTest
	_ = json.Unmarshal(data, &result)
	if result.Updated != 1 || result.Added != 0 {
		t.Fatalf("覆盖统计不符: %+v", result)
	}
	view, _ := env.s.pool.View("seed")
	if view.Enabled || view.Priority != 7 || view.Weight != 30 {
		t.Fatalf("人工设置未保留: enabled=%v priority=%d weight=%d", view.Enabled, view.Priority, view.Weight)
	}
	raw := findAccount(readAccountsFile(t, env.s.cfg.Accounts.AccountsFile()), "seed")
	if string(raw["access_token"]) != `"new-token"` || string(raw["custom_field"]) != `"keep"` {
		t.Fatalf("凭证或未知字段未正确更新: %+v", raw)
	}
}

func TestAdminImportInvalidFileReturnsCounts(t *testing.T) {
	const key = "admin-import-key"
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, nil,
		func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })
	resp, data := doAdminImport(t, env, key, nil, []importTestFile{
		{Field: "files", Name: "bad.json", Data: []byte(`{"secret-token":"must-not-leak"`)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("坏文件应返回 200 + invalid 统计，实际 %d body=%s", resp.StatusCode, data)
	}
	var result importResponseForTest
	_ = json.Unmarshal(data, &result)
	if result.Total != 1 || result.Invalid != 1 || len(result.Errors) != 1 {
		t.Fatalf("invalid 统计不符: %+v", result)
	}
	if strings.Contains(string(data), "must-not-leak") {
		t.Fatalf("错误响应泄露输入内容: %s", data)
	}
}

func TestAdminImportValidationAndMethod(t *testing.T) {
	const key = "admin-import-key"
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, nil,
		func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := doAdminImport(t, env, key, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无文件状态码 = %d, body=%s", resp.StatusCode, data)
	}

	req, err := http.NewRequest(http.MethodGet, env.hs.URL+"/admin/api/accounts/import", nil)
	if err != nil {
		t.Fatalf("构造 GET: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET 请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d, 期望 405", resp.StatusCode)
	}
}

type importResponseForTest struct {
	DryRun    bool     `json:"dry_run"`
	Total     int      `json:"total"`
	Added     int      `json:"added"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Invalid   int      `json:"invalid"`
	Errors    []string `json:"errors"`
	Truncated bool     `json:"errors_truncated"`
}

type importTestFile struct {
	Field string
	Name  string
	Data  []byte
}

func doAdminImport(t *testing.T, env *testEnv, key string, fields map[string]string, files []importTestFile) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("写导入字段: %v", err)
		}
	}
	for _, file := range files {
		part, err := writer.CreateFormFile(file.Field, file.Name)
		if err != nil {
			t.Fatalf("创建上传 part: %v", err)
		}
		if _, err := part.Write(file.Data); err != nil {
			t.Fatalf("写上传 part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, env.hs.URL+"/admin/api/accounts/import", &body)
	if err != nil {
		t.Fatalf("构造导入请求: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发送导入请求: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func marshalTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化测试 JSON: %v", err)
	}
	return data
}

func makeTestZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("创建 ZIP 条目: %v", err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatalf("写入 ZIP 条目: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 ZIP: %v", err)
	}
	return buffer.Bytes()
}
