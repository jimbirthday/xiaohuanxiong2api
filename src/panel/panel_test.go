package panel

import (
	"strings"
	"testing"
)

// TestPanelAuthBootstrapSafety 覆盖面板登录引导与凭据处理的关键约束。
func TestPanelAuthBootstrapSafety(t *testing.T) {
	html := string(HTML())
	required := []string{
		`id="admin-login"`,
		`id="admin-password" type="password"`,
		`id="logout"`,
		`credentials: "same-origin"`,
		`X-CSRF-Token`,
		`/admin/api/auth/session`,
		`if (!authenticated) return Promise.resolve(false);`,
		`var authEpoch = 0;`,
		`var bootstrapEpoch = authEpoch;`,
		`if (bootstrapEpoch !== authEpoch) return;`,
		`if (loginEpoch !== authEpoch) return;`,
		`requestEpoch === authEpoch`,
	}
	for _, fragment := range required {
		if !strings.Contains(html, fragment) {
			t.Fatalf("面板缺少登录/鉴权关键片段: %s", fragment)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("面板不得把会话状态写入 %s", forbidden)
		}
	}
	if strings.Contains(html, "<script src=") || strings.Contains(html, "<link rel=\"stylesheet\"") {
		t.Fatal("面板必须保持单文件、无外部前端依赖")
	}
}

// TestPanelBulkImportSafety 覆盖批量导入的 multipart、CSRF、错误渲染与刷新契约。
func TestPanelBulkImportSafety(t *testing.T) {
	html := string(HTML())
	required := []string{
		`id="toggle-import"`,
		`id="import-account-form"`,
		`<input id="import-files" type="file" accept=".json,.zip" multiple>`,
		`id="import-overwrite"`,
		`id="import-dry-run"`,
		`id="submit-import"`,
		`id="import-result"`,
		`new FormData()`,
		`formData.append("files", files[i], files[i].name);`,
		`formData.append("overwrite", overwrite ? "true" : "false");`,
		`formData.append("dry_run", dryRun ? "true" : "false");`,
		`requestFormData("/admin/api/accounts/import", formData)`,
		`credentials: "same-origin"`,
		`headers["X-CSRF-Token"] = CSRF_TOKEN;`,
		`预演，未写盘`,
		`name.textContent = files[i].name;`,
		`item.textContent = String(message);`,
		`Promise.all([loadAccounts(), loadStatus()])`,
	}
	for _, fragment := range required {
		if !strings.Contains(html, fragment) {
			t.Fatalf("面板缺少批量导入关键片段: %s", fragment)
		}
	}

	for _, forbidden := range []string{"localStorage", "sessionStorage", "document.cookie", "innerHTML"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("面板不得使用 %s 处理导入状态或渲染内容", forbidden)
		}
	}

	start := strings.Index(html, "function requestFormData")
	if start < 0 {
		t.Fatal("面板缺少 requestFormData")
	}
	end := strings.Index(html[start:], "function getJSON")
	if end < 0 {
		t.Fatal("无法定位 requestFormData 边界")
	}
	formDataHelper := html[start : start+end]
	// multipart boundary 必须由浏览器生成，手动设置 Content-Type 会破坏上传。
	if strings.Contains(formDataHelper, "Content-Type") {
		t.Fatal("FormData 请求不得手动设置 Content-Type")
	}
}
