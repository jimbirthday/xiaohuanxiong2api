// Package auth 提供单管理员的密码、API Key 与会话鉴权。
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	sessionSecretSize = 32
	apiKeyScheme      = "xhx_"
)

var errInvalidCredentials = errors.New("用户名或密码错误")

// Credentials 用于首次建库时向调用方展示一次性明文凭据。
type Credentials struct {
	Username  string
	Password  string
	APIKey    string
	Generated bool
}

// Session 是校验通过后返回给调用方的会话信息。
type Session struct {
	Username  string
	CSRF      string
	ExpiresAt time.Time
}

// Manager 持有认证存储与内存会话表。
type Manager struct {
	sessionTTL time.Duration
	mu         sync.RWMutex
	store      authStore
	sessions   map[[sha256.Size]byte]sessionRecord
}

// New 加载认证存储；文件不存在时创建唯一的 admin 管理员并持久化。
func New(path string, sessionTTL time.Duration) (*Manager, Credentials, error) {
	if strings.TrimSpace(path) == "" {
		return nil, Credentials{}, errors.New("认证存储路径不能为空")
	}

	store, exists, err := readStore(path)
	if err != nil {
		return nil, Credentials{}, err
	}

	credentials := Credentials{}
	if exists {
		credentials.Username = store.Username
	} else {
		store, credentials, err = createStore(time.Now().UTC())
		if err != nil {
			return nil, Credentials{}, err
		}
		// 首次创建必须成功原子落盘，否则不向调用方返回可用凭据。
		if err := writeStore(path, store); err != nil {
			return nil, Credentials{}, err
		}
	}

	return &Manager{
		sessionTTL: sessionTTL,
		store:      store,
		sessions:   make(map[[sha256.Size]byte]sessionRecord),
	}, credentials, nil
}

// Login 校验管理员密码并创建新的内存会话。
func (m *Manager) Login(username, password string) (token string, csrf string, expiresAt time.Time, err error) {
	m.mu.RLock()
	storedUsername := m.store.Username
	storedPassword := m.store.Password
	m.mu.RUnlock()

	passwordOK, err := verifyPassword(storedPassword, password)
	if err != nil {
		return "", "", time.Time{}, err
	}
	usernameOK := constantTimeCompare([]byte(storedUsername), []byte(username))
	// 用户名和密码都完成校验后再统一失败，降低用户名枚举的时序差异。
	if usernameOK != 1 || !passwordOK {
		return "", "", time.Time{}, errInvalidCredentials
	}

	token, err = randomRawURL(sessionSecretSize)
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrf, err = randomRawURL(sessionSecretSize)
	if err != nil {
		return "", "", time.Time{}, err
	}

	now := time.Now().UTC()
	expiresAt = now.Add(m.sessionTTL)
	m.mu.Lock()
	m.cleanupExpiredSessionsLocked(now)
	m.sessions[sessionTokenHash(token)] = sessionRecord{
		username:  storedUsername,
		csrf:      csrf,
		expiresAt: expiresAt,
	}
	m.mu.Unlock()

	return token, csrf, expiresAt, nil
}

// ValidateSession 校验会话是否有效，并顺带清理已过期会话。
func (m *Manager) ValidateSession(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}

	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleanupExpiredSessionsLocked(now)
	record, ok := m.sessions[sessionTokenHash(token)]
	if !ok {
		return Session{}, false
	}

	return Session{
		Username:  record.username,
		CSRF:      record.csrf,
		ExpiresAt: record.expiresAt,
	}, true
}

// Logout 删除指定 token 对应的会话。
func (m *Manager) Logout(token string) {
	if token == "" {
		return
	}

	m.mu.Lock()
	delete(m.sessions, sessionTokenHash(token))
	m.mu.Unlock()
}

// VerifyAPIKey 校验 API Key 是否匹配任一已保存的摘要。
func (m *Manager) VerifyAPIKey(key string) bool {
	candidateHash := apiKeyHash(key)

	m.mu.RLock()
	defer m.mu.RUnlock()

	matched := 0
	// 遍历全部条目并累计比较结果，避免命中位置泄露时序信息。
	for _, record := range m.store.APIKeys {
		matched |= constantTimeCompare([]byte(candidateHash), []byte(record.SHA256))
	}
	return matched == 1
}

// 凭据比较统一使用常量时间实现，避免普通比较泄露匹配前缀。
func constantTimeCompare(left, right []byte) int {
	return subtle.ConstantTimeCompare(left, right)
}

func apiKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}
