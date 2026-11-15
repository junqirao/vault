package app

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"vault/internal/apperr"
)

// steamTOTPCode 生成 Steam Guard 手机令牌验证码。
//
// Steam 的算法与标准 TOTP 的 HMAC/时间步一致，但输出不是 6 位数字：
// 它取完整的 32 位动态截断值，再按 Steam 的 26 字符表生成 **5 位字母数字**。
// 直接把标准 6 位数字传给 SteamCMD 会被判定为错误验证码。
func steamTOTPCode(secret string, t time.Time) (string, error) {
	raw := strings.TrimSpace(secret)
	if raw == "" {
		return "", apperr.New("steam.account_guard_missing", 409)
	}
	// Steam Desktop Authenticator / steamguard-cli 常给 base64 的 shared_secret；
	// 手工填写的 TOTP 密钥通常是 base32。两种都接受。
	base32Secret := strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(raw), " ", ""), "-", "")
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(base32Secret)
	if err != nil {
		// 兼容带 padding 的 base32。
		key, err = base32.StdEncoding.DecodeString(base32Secret)
	}
	if err != nil {
		for _, decode := range []func(string) ([]byte, error){
			base64.StdEncoding.DecodeString,
			base64.RawStdEncoding.DecodeString,
			base64.URLEncoding.DecodeString,
			base64.RawURLEncoding.DecodeString,
		} {
			if candidate, decodeErr := decode(raw); decodeErr == nil {
				key = candidate
				err = nil
				break
			}
		}
	}
	if err != nil {
		return "", apperr.New("steam.account_guard_invalid", 400).WithCause(err)
	}
	counter := uint64(t.Unix() / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	if _, err := mac.Write(buf[:]); err != nil {
		return "", err
	}
	sum := mac.Sum(nil)
	if len(sum) != sha1.Size {
		return "", fmt.Errorf("steam: TOTP 摘要长度异常")
	}
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	const alphabet = "23456789BCDFGHJKMNPQRTVWXY"
	var out strings.Builder
	out.Grow(5)
	for i := 0; i < 5; i++ {
		out.WriteByte(alphabet[code%uint32(len(alphabet))])
		code /= uint32(len(alphabet))
	}
	return out.String(), nil
}
