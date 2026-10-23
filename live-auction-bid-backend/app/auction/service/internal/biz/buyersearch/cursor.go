package buyersearch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

func EncodeCursor(p CursorPayload, key []byte) (string, error) {
	if len(key) < 32 || p.SnapshotID == "" || p.QueryHash == "" || p.GenerationID == "" || p.Offset < 0 || p.Offset > 200 {
		return "", ErrInvalidCursor
	}
	b, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func DecodeCursor(cursor string, key []byte, now time.Time) (CursorPayload, error) {
	var p CursorPayload
	if len(key) < 32 || len(cursor) > 2048 {
		return p, ErrInvalidCursor
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		return p, ErrInvalidCursor
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return p, ErrInvalidCursor
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return p, ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return p, ErrInvalidCursor
	}
	if json.Unmarshal(b, &p) != nil || p.SnapshotID == "" || p.QueryHash == "" || p.GenerationID == "" || p.Offset < 0 || p.Offset > 200 {
		return p, ErrInvalidCursor
	}
	if p.ExpiresAtUnixMs <= now.UnixMilli() {
		return p, ErrCursorExpired
	}
	if p.ExpiresAtUnixMs > now.Add(2*time.Minute).UnixMilli() {
		return p, ErrInvalidCursor
	}
	return p, nil
}
