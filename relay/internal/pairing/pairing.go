// Package pairing makes device tokens and one-time pairing codes, and prints the pairing QR code.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
)

const (
	// TokenPrefix starts every device token.
	TokenPrefix = "trs_"
	// CodeTTL is how long a pairing code is valid.
	CodeTTL = 10 * time.Minute
	// codeAlphabet is Crockford's base32 (no I, L, O, U): easy to read aloud and type.
	codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	codeLen      = 12
)

// NewToken returns a new device token ("trs_" + base64url of 32 random bytes) and its SHA-256.
func NewToken() (token string, hash []byte) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token)
}

// Hash is the SHA-256 the store keeps for tokens and codes.
func Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

var tokenRe = regexp.MustCompile(`^trs_[A-Za-z0-9_-]{43}$`)

// WellFormedToken reports whether s looks like a device token.
func WellFormedToken(s string) bool { return tokenRe.MatchString(s) }

// NewCode returns a new pairing code formatted XXXX-XXXX-XXXX (60 bits) and the hash of its normalized form.
func NewCode() (code string, hash []byte) {
	b := make([]byte, codeLen)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[v&31])
	}
	code = sb.String()
	norm, _ := NormalizeCode(code)
	return code, Hash(norm)
}

// NormalizeCode upper-cases a typed or scanned code and drops dashes and spaces. ok is false if it can't be a code.
func NormalizeCode(s string) (string, bool) {
	if len(s) > 64 {
		return "", false
	}
	var sb strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ':
		case strings.ContainsRune(codeAlphabet, r):
			sb.WriteRune(r)
		default:
			return "", false
		}
	}
	if sb.Len() != codeLen {
		return "", false
	}
	return sb.String(), true
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports whether s is an acceptable device name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// QRPayload is what the app scans.
func QRPayload(publicURL, code string) string {
	b, _ := json.Marshal(struct {
		V    int    `json:"v"`
		URL  string `json:"url"`
		Code string `json:"code"`
	}{1, publicURL, code})
	return string(b)
}

// Print writes the QR code and the same data as text to w (the operator's terminal, never the logs).
func Print(w io.Writer, name, publicURL, code string, expires time.Time) {
	qrterminal.GenerateWithConfig(QRPayload(publicURL, code), qrterminal.Config{
		Level: qrterminal.M, Writer: w, HalfBlocks: true,
		BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar: qrterminal.WHITE_WHITE, BlackWhiteChar: qrterminal.BLACK_WHITE, QuietZone: 2,
	})
	fmt.Fprintf(w, "\nDevice:  %s\nRelay:   %s\nCode:    %s\nExpires: %s (single use)\n",
		name, publicURL, code, expires.Local().Format("2006-01-02 15:04:05 MST"))
}
