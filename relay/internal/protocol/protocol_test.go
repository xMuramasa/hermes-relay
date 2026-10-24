package protocol

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoProtocol = "../../../protocol"

func TestEmbeddedSchemaInSync(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(repoProtocol, "schema.json"))
	if err != nil {
		t.Fatalf("read protocol/schema.json: %v", err)
	}
	if !bytes.Equal(want, SchemaJSON) {
		t.Fatal("relay/internal/protocol/schema.json differs from protocol/schema.json; " +
			"sync it with: cp protocol/schema.json relay/internal/protocol/schema.json")
	}
}

func examples(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoProtocol, "examples", dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples in %s: %v", dir, err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

func TestValidExamples(t *testing.T) {
	v := MustValidator()
	types := map[string]bool{}
	for name, raw := range examples(t, "valid") {
		if err := v.ValidateDetail(raw); err != nil {
			t.Errorf("%s should be valid: %v", name, err)
		}
		types[strings.SplitN(strings.TrimSuffix(name, ".json"), "-", 2)[0]] = true
	}
	for _, typ := range []string{"auth", "subscribe", "unsubscribe", "prompt", "stop", "steer", "approve", "ping",
		"ready", "event", "run", "ack", "error", "pong"} {
		if !types[typ] {
			t.Errorf("no valid example for type %q", typ)
		}
	}
}

func TestInvalidExamples(t *testing.T) {
	v := MustValidator()
	for name, raw := range examples(t, "invalid") {
		if err := v.Validate(raw); err == nil {
			t.Errorf("%s should be invalid", name)
		}
	}
}

func TestValidateCodes(t *testing.T) {
	v := MustValidator()
	cases := map[string]string{
		"prompt-oversize-text.json":  CodeTooLarge,
		"steer-oversize-text.json":   CodeTooLarge,
		"approve-choice-always.json": CodeBadRequest,
		"prompt-extra-field.json":    CodeBadRequest,
		"unknown-type.json":          CodeBadRequest,
	}
	ex := examples(t, "invalid")
	for name, code := range cases {
		if got := CodeOf(v.Validate(ex[name])); got != code {
			t.Errorf("%s: code %q, want %q", name, got, code)
		}
	}
	if got := CodeOf(v.Validate([]byte("{nope"))); got != CodeBadRequest {
		t.Errorf("broken JSON: %q", got)
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"api_1792800970_b3a06956", "run_007a9dd359a646d5af971aeb2dddb027", "20261023_211905_bd8221",
		"6f1c2a9e-1b7d-4c1e-9a35-2f0c7d3b8e41"} {
		if !ValidID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "..", ".x", "a/b", "a b", "a?b", strings.Repeat("a", 129), "é"} {
		if ValidID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
