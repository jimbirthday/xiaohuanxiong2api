package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	authStoreVersion          = 1
	adminUsername             = "admin"
	adminPasswordEnv          = "XHX_ADMIN_PASSWORD"
	minAdminPasswordLength    = 12
	passwordAlgorithm         = "pbkdf2-hmac-sha256"
	passwordIterations        = 600000
	passwordSaltSize          = 16
	passwordDigestSize        = 32
	apiKeyRandomSize          = 32
	apiKeyPrefixLength        = 8
	generatedAdminPasswordLen = 32
)

type passwordRecord struct {
	Algo       string `json:"algo"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

type apiKeyRecord struct {
	Prefix    string    `json:"prefix"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

type authStore struct {
	Version  int            `json:"version"`
	Username string         `json:"username"`
	Password passwordRecord `json:"password"`
	APIKeys  []apiKeyRecord `json:"api_keys"`
}

func readStore(path string) (authStore, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return authStore{}, false, nil
	}
	if err != nil {
		return authStore{}, false, fmt.Errorf("读取认证存储失败: %w", err)
	}

	var store authStore
	if err := json.Unmarshal(data, &store); err != nil {
		return authStore{}, false, fmt.Errorf("解析认证存储失败: %w", err)
	}
	if err := validateStore(store); err != nil {
		return authStore{}, false, err
	}
	return store, true, nil
}

func validateStore(store authStore) error {
	if store.Version != authStoreVersion {
		return fmt.Errorf("不支持的认证存储版本: %d", store.Version)
	}
	if strings.TrimSpace(store.Username) == "" {
		return errors.New("认证存储缺少用户名")
	}
	if _, _, err := decodePasswordRecord(store.Password); err != nil {
		return err
	}

	for _, record := range store.APIKeys {
		if len(record.Prefix) != apiKeyPrefixLength {
			return errors.New("认证存储中的 API Key 前缀无效")
		}
		digest, err := hex.DecodeString(record.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return errors.New("认证存储中的 API Key 摘要无效")
		}
	}
	return nil
}

func createStore(now time.Time) (authStore, Credentials, error) {
	password, err := initialAdminPassword()
	if err != nil {
		return authStore{}, Credentials{}, err
	}
	salt, err := randomBytes(passwordSaltSize)
	if err != nil {
		return authStore{}, Credentials{}, err
	}
	passwordDigest, err := derivePassword(password, salt)
	if err != nil {
		return authStore{}, Credentials{}, err
	}
	apiKeySecret, err := randomRawURL(apiKeyRandomSize)
	if err != nil {
		return authStore{}, Credentials{}, err
	}

	apiKey := apiKeyScheme + apiKeySecret
	// 持久化只记录口令派生值和 API Key 摘要，绝不落盘明文口令或 Key。
	store := authStore{
		Version:  authStoreVersion,
		Username: adminUsername,
		Password: passwordRecord{
			Algo:       passwordAlgorithm,
			Iterations: passwordIterations,
			Salt:       base64.StdEncoding.EncodeToString(salt),
			Hash:       base64.StdEncoding.EncodeToString(passwordDigest),
		},
		APIKeys: []apiKeyRecord{newAPIKeyRecord(apiKey, now)},
	}
	credentials := Credentials{
		Username:  adminUsername,
		Password:  password,
		APIKey:    apiKey,
		Generated: true,
	}
	return store, credentials, nil
}

func initialAdminPassword() (string, error) {
	if value, exists := os.LookupEnv(adminPasswordEnv); exists {
		password := strings.TrimSpace(value)
		if utf8.RuneCountInString(password) < minAdminPasswordLength {
			return "", fmt.Errorf("%s 去空白后至少需要 %d 个字符", adminPasswordEnv, minAdminPasswordLength)
		}
		return password, nil
	}

	secret, err := randomRawURL(generatedAdminPasswordLen)
	if err != nil {
		return "", err
	}
	return secret, nil
}

func newAPIKeyRecord(key string, createdAt time.Time) apiKeyRecord {
	return apiKeyRecord{
		Prefix:    key[:apiKeyPrefixLength],
		SHA256:    apiKeyHash(key),
		CreatedAt: createdAt,
	}
}

func decodePasswordRecord(record passwordRecord) ([]byte, []byte, error) {
	if record.Algo != passwordAlgorithm || record.Iterations != passwordIterations {
		return nil, nil, errors.New("认证存储中的密码参数无效")
	}

	salt, err := base64.StdEncoding.DecodeString(record.Salt)
	if err != nil || len(salt) != passwordSaltSize {
		return nil, nil, errors.New("认证存储中的密码盐无效")
	}
	digest, err := base64.StdEncoding.DecodeString(record.Hash)
	if err != nil || len(digest) != passwordDigestSize {
		return nil, nil, errors.New("认证存储中的密码摘要无效")
	}
	return salt, digest, nil
}

func verifyPassword(record passwordRecord, password string) (bool, error) {
	salt, expectedDigest, err := decodePasswordRecord(record)
	if err != nil {
		return false, err
	}
	actualDigest, err := derivePassword(password, salt)
	if err != nil {
		return false, err
	}
	return constantTimeCompare(expectedDigest, actualDigest) == 1, nil
}

func derivePassword(password string, salt []byte) ([]byte, error) {
	digest, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordDigestSize)
	if err != nil {
		return nil, fmt.Errorf("派生密码摘要失败: %w", err)
	}
	return digest, nil
}

func randomBytes(size int) ([]byte, error) {
	if size <= 0 {
		return nil, errors.New("随机值长度必须为正数")
	}
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("生成安全随机值失败: %w", err)
	}
	return value, nil
}

func randomRawURL(size int) (string, error) {
	value, err := randomBytes(size)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func writeStore(path string, store authStore) error {
	payload, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("编码认证存储失败: %w", err)
	}
	payload = append(payload, '\n')

	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("创建认证存储目录失败: %w", err)
	}

	temporary, err := os.CreateTemp(parent, ".xhx-auth-*.tmp")
	if err != nil {
		return fmt.Errorf("创建认证存储临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	// 固定 0600 并先写临时文件再 rename，避免半写文件或权限漂移。
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("设置认证存储权限失败: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return fmt.Errorf("写入认证存储失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步认证存储失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭认证存储临时文件失败: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("替换认证存储文件失败: %w", err)
	}
	return nil
}
