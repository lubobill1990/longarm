package hub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
)

// Panel accounts (the hub's HTTP API sits behind a reverse proxy, so client
// certificates cannot be used there): a password (argon2id) plus a TOTP
// code (RFC 6238, SHA-1, 6 digits, 30 s, ±1 step, no reuse of a step).

const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB (OWASP minimum for argon2id)
	argonThreads = 1
	argonKeyLen  = 32
)

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	k := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(k))
}

func checkPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random base32 secret.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTP computes the code for a secret at time t.
func TOTP(secret string, t time.Time) (string, error) {
	return totpAt(secret, t.Unix()/30)
}

func totpAt(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000), nil
}

// checkTOTP returns the accepted time step (> last) or an error.
func checkTOTP(secret, code string, last int64, now time.Time) (int64, error) {
	step := now.Unix() / 30
	for _, s := range []int64{step, step - 1, step + 1} {
		want, err := totpAt(secret, s)
		if err != nil {
			return 0, err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(code))) == 1 {
			if s <= last {
				return 0, errors.New("code already used")
			}
			return s, nil
		}
	}
	return 0, errors.New("wrong code")
}

// TOTPURI is the otpauth:// URI for authenticator apps.
func TOTPURI(user, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=longarm-hub", url.PathEscape("longarm-hub:"+user), secret)
}

// AddUser creates (or with replace, resets) a panel account; it returns
// the TOTP secret to enroll in an authenticator app.
func AddUser(st *store.Store, name, password string, replace bool) (string, error) {
	if !pki.ValidName(name) {
		return "", fmt.Errorf("invalid user name %q", name)
	}
	if len(password) < 10 {
		return "", errors.New("the password must have at least 10 characters")
	}
	if _, err := st.GetUser(name); err == nil && !replace {
		return "", fmt.Errorf("user %s exists (use --reset to replace the password and TOTP secret)", name)
	}
	u := &store.User{Name: name, PassHash: hashPassword(password), TOTPSecret: NewTOTPSecret(), CreatedAt: time.Now().UTC()}
	return u.TOTPSecret, st.PutUser(u)
}

func sessionHash(tok string) string {
	s := sha256.Sum256([]byte("longarm-hub session:" + tok))
	return hex.EncodeToString(s[:])
}
