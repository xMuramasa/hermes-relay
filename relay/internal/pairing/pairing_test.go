package pairing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToken(t *testing.T) {
	tok, hash := NewToken()
	if !strings.HasPrefix(tok, "trs_") || !WellFormedToken(tok) || len(hash) != 32 || !bytes.Equal(hash, Hash(tok)) {
		t.Fatalf("token %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, "trs_"))
	if err != nil || len(raw) != 32 {
		t.Fatal("token is not base64url of 32 bytes")
	}
	if tok2, _ := NewToken(); tok2 == tok {
		t.Fatal("tokens repeat")
	}
	for _, bad := range []string{"", "trs_", "trs_short", "abc_" + strings.Repeat("A", 43), "trs_" + strings.Repeat("A", 42) + "!"} {
		if WellFormedToken(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCode(t *testing.T) {
	code, hash := NewCode()
	if len(code) != 14 || code[4] != '-' || code[9] != '-' {
		t.Fatalf("code %q", code)
	}
	for _, typed := range []string{code, strings.ToLower(code), strings.ReplaceAll(code, "-", " ")} {
		n, ok := NormalizeCode(typed)
		if !ok || !bytes.Equal(Hash(n), hash) {
			t.Errorf("%q does not normalize to the code", typed)
		}
	}
	for _, bad := range []string{"", "ABCD", "ABCD-EFGH-IJKL", "ABCD-EFGH-JKMN-P", strings.Repeat("A", 100)} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNames(t *testing.T) {
	for _, ok := range []string{"pixel-8", "Mac.mini", "a"} {
		if !ValidName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "x/y", strings.Repeat("a", 65)} {
		if ValidName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestQR(t *testing.T) {
	var p struct {
		V         int
		URL, Code string
	}
	if err := json.Unmarshal([]byte(QRPayload("https://relay.example", "ABCD-EFGH-JKMN")), &p); err != nil ||
		p.V != 1 || p.URL != "https://relay.example" || p.Code != "ABCD-EFGH-JKMN" {
		t.Fatalf("%+v %v", p, err)
	}
	var out bytes.Buffer
	Print(&out, "pixel-8", "https://relay.example", "ABCD-EFGH-JKMN", time.Now().Add(CodeTTL))
	if !strings.Contains(out.String(), "▀") || !strings.Contains(out.String(), "Code:    ABCD-EFGH-JKMN") ||
		!strings.Contains(out.String(), "single use") {
		t.Fatal(out.String())
	}
}
