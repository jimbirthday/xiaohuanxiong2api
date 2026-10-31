package auth

import (
	"crypto/sha256"
	"time"
)

type sessionRecord struct {
	username  string
	csrf      string
	expiresAt time.Time
}

// 会话表以 token 的 SHA-256 摘要作为键，内存中不保留可重放的明文 token。
func sessionTokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

// 登录和校验时顺便清理过期会话，避免为轻量会话表增加后台协程。
func (m *Manager) cleanupExpiredSessionsLocked(now time.Time) {
	for key, record := range m.sessions {
		if !now.Before(record.expiresAt) {
			delete(m.sessions, key)
		}
	}
}
