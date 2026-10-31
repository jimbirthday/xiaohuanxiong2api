package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAdminPassword = "test-admin-password-2026"

// TestNewHashesSecretsAndSetsPrivatePermissions 覆盖首次建库的密钥落盘与文件权限。
func TestNewHashesSecretsAndSetsPrivatePermissions(t *testing.T) {
	t.Setenv(adminPasswordEnv, testAdminPassword)
	authPath := filepath.Join(t.TempDir(), "nested", "auth.json")
	manager, credentials, err := New(authPath, 15*time.Minute)
	if err != nil {
		t.Fatalf("首次创建认证存储失败: %v", err)
	}
	if !credentials.Generated || credentials.Username != adminUsername || credentials.Password != testAdminPassword {
		t.Fatalf("首次凭据不符: %+v", credentials)
	}
	if !strings.HasPrefix(credentials.APIKey, apiKeyScheme) ||
		len(credentials.APIKey) != len(apiKeyScheme)+base64.RawURLEncoding.EncodedLen(apiKeyRandomSize) {
		t.Fatalf("API Key 格式不符: %q", credentials.APIKey)
	}

	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("读取认证文件状态失败: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("认证文件权限 = %o, 期望 600", got)
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("读取认证文件失败: %v", err)
	}
	if bytes.Contains(raw, []byte(credentials.Password)) {
		t.Fatal("认证文件包含管理员密码明文")
	}
	if bytes.Contains(raw, []byte(credentials.APIKey)) {
		t.Fatal("认证文件包含 API Key 明文")
	}

	var stored authStore
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("解析认证文件失败: %v", err)
	}
	if stored.Version != authStoreVersion || stored.Username != adminUsername {
		t.Fatalf("认证文件版本或用户名不符: %+v", stored)
	}
	if stored.Password.Algo != passwordAlgorithm || stored.Password.Iterations != passwordIterations {
		t.Fatalf("PBKDF2 参数不符: %+v", stored.Password)
	}
	salt, err := base64.StdEncoding.DecodeString(stored.Password.Salt)
	if err != nil || len(salt) != passwordSaltSize {
		t.Fatalf("密码盐无效: len=%d err=%v", len(salt), err)
	}
	digest, err := base64.StdEncoding.DecodeString(stored.Password.Hash)
	if err != nil || len(digest) != passwordDigestSize {
		t.Fatalf("密码摘要无效: len=%d err=%v", len(digest), err)
	}
	if len(stored.APIKeys) != 1 ||
		stored.APIKeys[0].Prefix != credentials.APIKey[:apiKeyPrefixLength] ||
		stored.APIKeys[0].SHA256 != apiKeyHash(credentials.APIKey) {
		t.Fatalf("API Key 摘要记录不符: %+v", stored.APIKeys)
	}

	reloaded, existingCredentials, err := New(authPath, 15*time.Minute)
	if err != nil {
		t.Fatalf("重新加载认证存储失败: %v", err)
	}
	if existingCredentials.Generated || existingCredentials.Password != "" || existingCredentials.APIKey != "" {
		t.Fatalf("已有认证文件不应返回明文凭据: %+v", existingCredentials)
	}

	if token, csrf, _, err := reloaded.Login(adminUsername, "wrong-"+testAdminPassword); !errors.Is(err, errInvalidCredentials) || token != "" || csrf != "" {
		t.Fatalf("错误密码登录结果不符: token=%q csrf=%q err=%v", token, csrf, err)
	}

	const sessionTTL = 15 * time.Minute
	beforeLogin := time.Now()
	token, csrf, expiresAt, err := reloaded.Login(adminUsername, testAdminPassword)
	if err != nil {
		t.Fatalf("正确密码登录失败: %v", err)
	}
	if len(token) != base64.RawURLEncoding.EncodedLen(sessionSecretSize) || len(csrf) != len(token) || token == csrf {
		t.Fatalf("会话 token/CSRF 格式不符: token_len=%d csrf_len=%d", len(token), len(csrf))
	}
	if expiresAt.Before(beforeLogin.Add(sessionTTL-time.Minute)) || expiresAt.After(beforeLogin.Add(sessionTTL+time.Minute)) {
		t.Fatalf("会话过期时间不符: %v", expiresAt)
	}

	reloaded.mu.RLock()
	_, storedByHash := reloaded.sessions[sessionTokenHash(token)]
	reloaded.mu.RUnlock()
	if !storedByHash {
		t.Fatal("会话表未按 token SHA-256 摘要保存")
	}
	session, ok := reloaded.ValidateSession(token)
	if !ok || session.Username != adminUsername || session.CSRF != csrf || !session.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("有效会话校验不符: %+v ok=%v", session, ok)
	}
	if !manager.VerifyAPIKey(credentials.APIKey) || manager.VerifyAPIKey(credentials.APIKey+"x") {
		t.Fatal("API Key 校验结果不符")
	}

	reloaded.Logout(token)
	if _, ok := reloaded.ValidateSession(token); ok {
		t.Fatal("注销后的会话仍然有效")
	}

	expiredToken := "expired-session-token-for-test"
	expiredKey := sessionTokenHash(expiredToken)
	reloaded.mu.Lock()
	reloaded.sessions[expiredKey] = sessionRecord{
		username: adminUsername, csrf: "expired-csrf", expiresAt: time.Now().Add(-time.Second),
	}
	reloaded.mu.Unlock()
	if _, ok := reloaded.ValidateSession(expiredToken); ok {
		t.Fatal("过期会话仍然有效")
	}
	reloaded.mu.RLock()
	_, expiredStillStored := reloaded.sessions[expiredKey]
	reloaded.mu.RUnlock()
	if expiredStillStored {
		t.Fatal("过期会话未被惰性清理")
	}

}

// TestNewRejectsShortAdminPassword 覆盖环境变量密码的最小长度校验。
func TestNewRejectsShortAdminPassword(t *testing.T) {
	t.Setenv(adminPasswordEnv, "  too-short  ")
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if _, _, err := New(authPath, time.Minute); err == nil || !strings.Contains(err.Error(), "至少需要 12 个字符") {
		t.Fatalf("短密码应被拒绝，实际错误: %v", err)
	}
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("短密码失败后不应创建认证文件: %v", err)
	}
}
