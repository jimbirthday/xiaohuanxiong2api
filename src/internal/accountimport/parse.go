package accountimport

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

var browserExportKeys = []string{"cookies_raw", "localStorage", "jwt", "phone"}

func looksLikeBrowserExport(root map[string]json.RawMessage) bool {
	for _, key := range browserExportKeys {
		if _, ok := root[key]; ok {
			return true
		}
	}
	return false
}

func normalizeStandard(raw json.RawMessage) (Item, error) {
	var account map[string]json.RawMessage
	if err := json.Unmarshal(raw, &account); err != nil {
		return Item{}, errors.New("账号记录必须是 JSON 对象")
	}

	token := firstRawString(account["access_token"], account["accessToken"])
	if token == "" {
		return Item{}, errors.New("缺少 access_token")
	}
	claims := decodeJWT(token)
	userID := firstRawString(account["user_id"], account["userId"], account["uid"], account["userID"])
	if userID == "" {
		userID = claims.UserID
	}
	id := firstRawString(account["id"])
	if id == "" {
		id = firstNonBlank(userID, firstRawString(account["phone"]))
	}
	if id == "" {
		return Item{}, errors.New("缺少可确定账号 ID 的字段（id/user_id/phone）")
	}

	setRawString(account, "id", id)
	if userID != "" && firstRawString(account["user_id"]) == "" {
		setRawString(account, "user_id", userID)
	}
	if _, ok := account["enabled"]; !ok {
		account["enabled"] = json.RawMessage("true")
	}
	if _, ok := account["priority"]; !ok {
		account["priority"] = json.RawMessage("100")
	}
	if _, ok := account["weight"]; !ok {
		account["weight"] = json.RawMessage("100")
	}
	if _, ok := account["expires_at"]; !ok && claims.ExpiresAt > 0 {
		account["expires_at"] = json.RawMessage(strconv.FormatInt(claims.ExpiresAt, 10))
	}

	data, err := json.Marshal(account)
	if err != nil {
		return Item{}, errors.New("账号记录序列化失败")
	}
	return Item{ID: id, JSON: data}, nil
}

func parseBrowserExport(root map[string]json.RawMessage) (Item, error) {
	cookies := parseCookieHeader(rawString(root["cookies_raw"]))
	local := parseLocalStorage(root["localStorage"])

	token := firstNonBlank(
		cookieValue(cookies, "xhx_token"),
		local.first("xhx_token"),
		firstJWT(root["jwt"]),
	)
	if token == "" {
		return Item{}, errors.New("缺少可用的 xhx_token（Cookie、localStorage、jwt 均未找到）")
	}
	claims := decodeJWT(token)

	userID := firstNonBlank(cookieValue(cookies, "user_id"), claims.UserID)
	phone := rawString(root["phone"])
	id := firstNonBlank(userID, phone)
	if id == "" {
		return Item{}, errors.New("缺少 user_id/phone，无法确定账号 ID")
	}
	if userID == "" {
		userID = id
	}
	name := firstNonBlank(
		cookieValue(cookies, "user_name"),
		local.first("user_name", "userName", "nickname", "user_nickname", "name"),
		phone,
		id,
	)

	account := map[string]any{
		"id":           id,
		"name":         name,
		"user_id":      userID,
		"access_token": token,
		"enabled":      true,
		"priority":     100,
		"weight":       100,
	}
	if claims.ExpiresAt > 0 {
		account["expires_at"] = claims.ExpiresAt
	}
	if phone != "" {
		account["phone"] = phone
	}
	if officeIdentity := cookieValue(cookies, "office_identity"); officeIdentity != "" {
		account["office_identity"] = officeIdentity
	}
	if refreshToken := firstNonBlank(local.first("refresh_token", "refreshToken")); refreshToken != "" {
		account["refresh_token"] = refreshToken
	}

	data, err := json.Marshal(account)
	if err != nil {
		return Item{}, errors.New("账号记录序列化失败")
	}
	return Item{ID: id, JSON: data}, nil
}

type cookieJar map[string]string

func parseCookieHeader(raw string) cookieJar {
	jars := cookieJar{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		jars[name] = value
	}
	return jars
}

func cookieValue(jar cookieJar, name string) string {
	if value := strings.TrimSpace(jar[name]); value != "" {
		return value
	}
	for key, value := range jar {
		if strings.EqualFold(key, name) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type localStorage map[string]json.RawMessage

func parseLocalStorage(raw json.RawMessage) localStorage {
	if len(raw) == 0 {
		return localStorage{}
	}
	var values localStorage
	if err := json.Unmarshal(raw, &values); err != nil {
		return localStorage{}
	}
	return values
}

func (local localStorage) first(keys ...string) string {
	for _, key := range keys {
		if raw, ok := local[key]; ok {
			if value := rawString(raw); value != "" {
				return value
			}
			if value := findNestedString(raw, keys, 0); value != "" {
				return value
			}
		}
	}
	for _, raw := range local {
		if value := findNestedString(raw, keys, 0); value != "" {
			return value
		}
	}
	return ""
}

func findNestedString(raw json.RawMessage, keys []string, depth int) string {
	if depth >= 3 || len(raw) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	for _, key := range keys {
		if value := rawString(object[key]); value != "" {
			return value
		}
	}
	for _, value := range object {
		if nested := findNestedString(value, keys, depth+1); nested != "" {
			return nested
		}
	}
	return ""
}

func firstJWT(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if token := tokenFromRaw(raw); token != "" {
		return token
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return ""
	}
	for _, value := range list {
		if token := tokenFromRaw(value); token != "" {
			return token
		}
	}
	return ""
}

func tokenFromRaw(raw json.RawMessage) string {
	token := rawString(raw)
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if strings.Count(token, ".") == 2 && !strings.ContainsAny(token, " \t\r\n") {
		return token
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		for _, key := range []string{"token", "jwt", "access_token"} {
			if token := tokenFromRaw(object[key]); token != "" {
				return token
			}
		}
	}
	return ""
}

type jwtClaims struct {
	UserID    string
	ExpiresAt int64
}

func decodeJWT(token string) jwtClaims {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return jwtClaims{}
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return jwtClaims{}
	}
	result := jwtClaims{}
	for _, key := range []string{"user_id", "userId", "uid", "userID", "sub"} {
		if value := rawString(claims[key]); value != "" && value != "0" {
			result.UserID = value
			break
		}
	}
	result.ExpiresAt = rawUnixMillis(claims["exp"])
	return result
}

func rawUnixMillis(raw json.RawMessage) int64 {
	text := rawString(raw)
	if text == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return int64(seconds * 1000)
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

func firstRawString(values ...json.RawMessage) string {
	for _, value := range values {
		if text := rawString(value); text != "" {
			return text
		}
	}
	return ""
}

func setRawString(object map[string]json.RawMessage, key, value string) {
	data, _ := json.Marshal(value)
	object[key] = data
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
