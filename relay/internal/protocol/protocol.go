// Package protocol holds the WebSocket message types shared with the app and validates messages against the
// embedded copy of protocol/schema.json.
package protocol

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// SchemaJSON is a byte-identical copy of protocol/schema.json (a test enforces it).
//
//go:embed schema.json
var SchemaJSON []byte

// Error codes (schema $defs/error_code).
const (
	CodeUnauthorized      = "unauthorized"
	CodeBadRequest        = "bad_request"
	CodeTooLarge          = "too_large"
	CodeRateLimited       = "rate_limited"
	CodeBusy              = "busy"
	CodeNotFound          = "not_found"
	CodeReadOnly          = "read_only"
	CodeReplayGap         = "replay_gap"
	CodeHermesUnavailable = "hermes_unavailable"
	CodeHermesError       = "hermes_error"
	CodeInternal          = "internal"
)

// Event kinds (schema $defs/event_kind).
const (
	KindToolStarted       = "tool.started"
	KindToolCompleted     = "tool.completed"
	KindSubagentStarted   = "subagent.started"
	KindSubagentCompleted = "subagent.completed"
	KindApprovalRequested = "approval.requested"
	KindApprovalResolved  = "approval.resolved"
	KindAnswer            = "answer"
	KindRunCompleted      = "run.completed"
	KindRunFailed         = "run.failed"
	KindRunCancelled      = "run.cancelled"
	KindSteerQueued       = "steer.queued"
	KindReconnected       = "reconnected"
	KindUnknown           = "unknown"
)

// Run statuses (schema $defs/run_status).
const (
	StatusRunning            = "running"
	StatusWaitingForApproval = "waiting_for_approval"
	StatusCompleted          = "completed"
	StatusFailed             = "failed"
	StatusCancelled          = "cancelled"
)

// Terminal reports whether a run status is final.
func Terminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// Limits mirrored from the schema, for checks outside it.
const (
	MaxMessageBytes = 64 << 10
	MaxTextChars    = 32768
	MaxIDChars      = 128
	MaxErrorChars   = 512
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// ValidID reports whether s matches the schema's id definition.
func ValidID(s string) bool { return idRe.MatchString(s) }

// Error is a protocol error: a code from the schema and a short message safe to show and log.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds a protocol error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the protocol code of err, or internal.
func CodeOf(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return CodeInternal
}

// Inbound is any phone → relay message, decoded after schema validation.
type Inbound struct {
	Type        string `json:"type"`
	Token       string `json:"token,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	AfterSeq    *int64 `json:"after_seq,omitempty"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
	Text        string `json:"text,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Choice      string `json:"choice,omitempty"`
}

// Outbound messages (relay → phone).
type (
	Ready struct {
		Type          string `json:"type"`
		Device        string `json:"device"`
		ServerVersion string `json:"server_version"`
	}
	Event struct {
		Type      string          `json:"type"`
		SessionID string          `json:"session_id"`
		RunID     string          `json:"run_id"`
		Seq       int64           `json:"seq"`
		Kind      string          `json:"kind"`
		Data      json.RawMessage `json:"data"`
		TS        int64           `json:"ts"`
	}
	Run struct {
		Type      string `json:"type"`
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}
	Ack struct {
		Type        string `json:"type"`
		ClientMsgID string `json:"client_msg_id"`
		RunID       string `json:"run_id"`
		Replayed    bool   `json:"replayed"`
	}
	ErrorMsg struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Ref     string `json:"ref,omitempty"`
	}
	Pong struct {
		Type string `json:"type"`
	}
)

// NewError builds an error message; ref is dropped when it isn't a valid id.
func NewError(code, message, ref string) ErrorMsg {
	if !ValidID(ref) {
		ref = ""
	}
	if r := []rune(message); len(r) > MaxErrorChars {
		message = string(r[:MaxErrorChars])
	}
	return ErrorMsg{Type: "error", Code: code, Message: message, Ref: ref}
}

// Validator checks messages against the embedded schema.
type Validator struct {
	sch *jsonschema.Schema
}

// NewValidator compiles the embedded schema.
func NewValidator() (*Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(SchemaJSON))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, fmt.Errorf("add schema: %w", err)
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return &Validator{sch: sch}, nil
}

// MustValidator is NewValidator for tests and init paths where the embedded schema can't be broken.
func MustValidator() *Validator {
	v, err := NewValidator()
	if err != nil {
		panic(err)
	}
	return v
}

// Validate checks one raw message. The returned *Error is too_large when a length cap was hit, else bad_request.
// Its message never contains the message's content.
func (v *Validator) Validate(raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &Error{Code: CodeBadRequest, Message: "not valid JSON"}
	}
	if err := v.sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) && hasMaxLength(ve) {
			return &Error{Code: CodeTooLarge, Message: "a field is too long"}
		}
		return &Error{Code: CodeBadRequest, Message: "message does not match the protocol schema"}
	}
	return nil
}

// ValidateDetail is Validate with the validator's full explanation, for tests.
func (v *Validator) ValidateDetail(raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return v.sch.Validate(inst)
}

func hasMaxLength(ve *jsonschema.ValidationError) bool {
	switch ve.ErrorKind.(type) {
	case *kind.MaxLength:
		return true
	}
	for _, c := range ve.Causes {
		if hasMaxLength(c) {
			return true
		}
	}
	return false
}
