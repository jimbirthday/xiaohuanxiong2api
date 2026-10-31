package accountimport

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseBrowserExport(t *testing.T) {
	token := makeTestJWT(t, map[string]any{"exp": 1900000000, "sub": "jwt-user"})
	export := map[string]any{
		"phone":       "13800000000",
		"cookies_raw": "xhx_token=" + token + "; user_id=user-1; user_name=Alice%20W; office_identity=enterprise",
		"localStorage": map[string]any{
			"ignored": "value",
		},
		"jwt": []string{token},
	}
	data := marshalJSON(t, export)

	result := Parse([]Input{{Name: "xhx-123.json", Data: data}})
	if result.Total != 1 || result.Valid != 1 || result.Invalid != 0 || len(result.Errors) != 0 {
		t.Fatalf("统计不符: %+v", result)
	}
	account := decodedItem(t, result.Accounts[0])
	if got := account["id"]; got != "user-1" {
		t.Fatalf("id = %v", got)
	}
	if got := account["name"]; got != "Alice W" {
		t.Fatalf("name = %v", got)
	}
	if got := account["access_token"]; got != token {
		t.Fatalf("access_token 不符")
	}
	if got := account["expires_at"]; got != float64(1900000000000) {
		t.Fatalf("expires_at = %v", got)
	}
	if got := account["phone"]; got != "13800000000" {
		t.Fatalf("phone = %v", got)
	}
	if got := account["office_identity"]; got != "enterprise" {
		t.Fatalf("office_identity = %v", got)
	}
	if got := account["enabled"]; got != true {
		t.Fatalf("enabled = %v", got)
	}
	if got := account["priority"]; got != float64(100) {
		t.Fatalf("priority = %v", got)
	}
	if got := account["weight"]; got != float64(100) {
		t.Fatalf("weight = %v", got)
	}
}

func TestParseBrowserExportFallbackFields(t *testing.T) {
	token := makeTestJWT(t, map[string]any{"exp": "1900000001", "sub": "sub-user"})
	badJWT := "not-a-jwt"
	export := map[string]any{
		"phone":       "13900000000",
		"cookies_raw": "office_identity=org",
		"localStorage": map[string]any{
			"xhx_token": token,
			"profile":   map[string]any{"userName": "Local Name"},
		},
		"jwt": []string{badJWT, token},
	}
	data := marshalJSON(t, export)

	result := Parse([]Input{{Name: "fallback.json", Data: data}})
	if result.Valid != 1 || result.Invalid != 0 {
		t.Fatalf("统计不符: %+v", result)
	}
	account := decodedItem(t, result.Accounts[0])
	if got := account["id"]; got != "sub-user" {
		t.Fatalf("id = %v", got)
	}
	if got := account["name"]; got != "Local Name" {
		t.Fatalf("name = %v", got)
	}
	if got := account["expires_at"]; got != float64(1900000001000) {
		t.Fatalf("expires_at = %v", got)
	}
}

func TestParseBrowserExportPhoneFallbackID(t *testing.T) {
	token := makeTestJWT(t, map[string]any{"exp": 1900000000})
	export := map[string]any{
		"phone":       "13700000000",
		"cookies_raw": "xhx_token=" + token,
	}
	result := Parse([]Input{{Name: "phone.json", Data: marshalJSON(t, export)}})
	if result.Valid != 1 {
		t.Fatalf("统计不符: %+v", result)
	}
	account := decodedItem(t, result.Accounts[0])
	if got := account["id"]; got != "13700000000" {
		t.Fatalf("id = %v", got)
	}
	if got := account["name"]; got != "13700000000" {
		t.Fatalf("name = %v", got)
	}
	if got := account["user_id"]; got != "13700000000" {
		t.Fatalf("user_id = %v", got)
	}
}

func TestParseStandardAccountsPreservesUnknownFields(t *testing.T) {
	payload := map[string]any{
		"accounts": []map[string]any{
			{
				"id":             "a1",
				"name":           "账号一",
				"user_id":        "u1",
				"access_token":   "token-a1",
				"enabled":        false,
				"priority":       7,
				"weight":         55,
				"custom_field":   map[string]any{"keep": true},
				"cooldown_until": 123,
			},
			{
				"user_id":      "u2",
				"access_token": "token-a2",
			},
		},
	}
	result := Parse([]Input{{Name: "accounts.json", Data: marshalJSON(t, payload)}})
	if result.Total != 2 || result.Valid != 2 || result.Invalid != 0 {
		t.Fatalf("统计不符: %+v", result)
	}
	first := decodedItem(t, result.Accounts[0])
	if first["enabled"] != false || first["priority"] != float64(7) || first["weight"] != float64(55) {
		t.Fatalf("标准账号设置被改写: %#v", first)
	}
	if _, ok := first["custom_field"]; !ok {
		t.Fatalf("未知字段丢失: %#v", first)
	}
	second := decodedItem(t, result.Accounts[1])
	if second["id"] != "u2" || second["enabled"] != true || second["priority"] != float64(100) || second["weight"] != float64(100) {
		t.Fatalf("缺失字段未补默认值: %#v", second)
	}
}

func TestParseZIPMixedFilesAndRedactsCredentials(t *testing.T) {
	token := makeTestJWT(t, map[string]any{"exp": 1900000000, "sub": "zip-user"})
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	writeZIPFile(t, writer, "browser.json", marshalJSON(t, map[string]any{
		"phone":       "13600000000",
		"cookies_raw": "xhx_token=" + token + "; user_name=SecretCookieName",
	}))
	writeZIPFile(t, writer, "nested/accounts.json", marshalJSON(t, map[string]any{
		"accounts": []map[string]any{
			{"id": "z1", "access_token": "plain-token-1"},
			{"id": "z2", "access_token": "plain-token-2"},
		},
	}))
	writeZIPFile(t, writer, "broken.json", []byte("{not-json"))
	writeZIPFile(t, writer, "__MACOSX/._browser.json", []byte("ignored"))
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 ZIP: %v", err)
	}

	result := Parse([]Input{{Name: "batch.zip", Data: archive.Bytes()}})
	if result.Total != 4 || result.Valid != 3 || result.Invalid != 1 {
		t.Fatalf("统计不符: %+v", result)
	}
	allErrors := strings.Join(result.Errors, "\n")
	for _, secret := range []string{token, "SecretCookieName", "plain-token-1"} {
		if strings.Contains(allErrors, secret) {
			t.Fatalf("错误信息泄露凭证: %s", allErrors)
		}
	}
	if !strings.Contains(allErrors, "broken.json") {
		t.Fatalf("错误应包含来源文件: %s", allErrors)
	}
}

func TestParseRejectsMalformedAndEmptyInputs(t *testing.T) {
	tests := []struct {
		name  string
		input Input
	}{
		{name: "empty", input: Input{Name: "empty.json"}},
		{name: "invalid json", input: Input{Name: "bad.json", Data: []byte("{")}},
		{name: "unsupported object", input: Input{Name: "other.json", Data: []byte(`{"foo":"bar"}`)}},
		{name: "empty accounts", input: Input{Name: "accounts.json", Data: []byte(`{"accounts":[]}`)}},
		{name: "invalid zip", input: Input{Name: "bad.zip", Data: []byte("PKbad")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Parse([]Input{test.input})
			if result.Valid != 0 || result.Invalid != 1 || result.Total != 1 || len(result.Errors) != 1 {
				t.Fatalf("统计不符: %+v", result)
			}
		})
	}
}

func TestParseLimits(t *testing.T) {
	t.Run("request bytes", func(t *testing.T) {
		result := Parse([]Input{{Name: "huge.json", Data: make([]byte, MaxRequestBytes+1)}})
		if result.Invalid != 1 || !strings.Contains(strings.Join(result.Errors, " "), "32MiB") {
			t.Fatalf("未拒绝超大请求: %+v", result)
		}
	})

	t.Run("zip entry count", func(t *testing.T) {
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		for i := 0; i <= MaxZIPEntries; i++ {
			writeZIPFile(t, writer, fmt.Sprintf("f%d.json", i), []byte(`{}`))
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("关闭 ZIP: %v", err)
		}
		result := Parse([]Input{{Name: "many.zip", Data: archive.Bytes()}})
		if result.Invalid != 1 || !strings.Contains(strings.Join(result.Errors, " "), "200") {
			t.Fatalf("未拒绝条目超限: %+v", result)
		}
	})

	t.Run("zip entry bytes", func(t *testing.T) {
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		writeZIPFile(t, writer, "large.json", bytes.Repeat([]byte("x"), int(MaxZIPEntryBytes)+1))
		if err := writer.Close(); err != nil {
			t.Fatalf("关闭 ZIP: %v", err)
		}
		result := Parse([]Input{{Name: "large.zip", Data: archive.Bytes()}})
		if result.Invalid != 1 || !strings.Contains(strings.Join(result.Errors, " "), "8MiB") {
			t.Fatalf("未拒绝条目超限: %+v", result)
		}
	})

	t.Run("error cap", func(t *testing.T) {
		inputs := make([]Input, MaxErrorItems+5)
		for i := range inputs {
			inputs[i] = Input{Name: fmt.Sprintf("bad-%d.json", i), Data: []byte(`{}`)}
		}
		result := Parse(inputs)
		if len(result.Errors) != MaxErrorItems || !result.ErrorsTruncated {
			t.Fatalf("错误条数未截断: len=%d truncated=%v", len(result.Errors), result.ErrorsTruncated)
		}
	})
}

func makeTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("序列化 JWT claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化测试数据: %v", err)
	}
	return data
}

func decodedItem(t *testing.T, item Item) map[string]any {
	t.Helper()
	var account map[string]any
	if err := json.Unmarshal(item.JSON, &account); err != nil {
		t.Fatalf("解析导入项: %v", err)
	}
	return account
}

func writeZIPFile(t *testing.T, writer *zip.Writer, name string, data []byte) {
	t.Helper()
	file, err := writer.Create(name)
	if err != nil {
		t.Fatalf("创建 ZIP 条目: %v", err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatalf("写入 ZIP 条目: %v", err)
	}
}
