package proto_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/proto"
)

// gatewayHelloMsg is a field-for-field copy of the Bridge Gateway's actual helloMsg
// (services/bridge-gateway/internal/ws/registration.go) — Signature is a base64 string
// there (manually decoded), not a []byte, so this must NOT reuse proto.HelloMsg. Decoding
// mkonnect's marshaled output into this independent type proves wire compatibility.
type gatewayHelloMsg struct {
	Type              string `json:"type"`
	ConnectorID       string `json:"connector_id"`
	ProtocolVersion   string `json:"protocol_version"`
	ConnectorVersion  string `json:"connector_version"`
	RegistrationToken string `json:"registration_token"`
	Signature         string `json:"signature"`
}

// TestHelloMsgWireFormat verifies mkonnect's HelloMsg marshals into exactly what the
// gateway's helloMsg expects — a typo in a json tag here means every mkonnect connector
// silently fails to register.
func TestHelloMsgWireFormat(t *testing.T) {
	msg := proto.HelloMsg{
		Type:              "hello",
		ConnectorID:       "c1",
		ProtocolVersion:   "v1",
		ConnectorVersion:  "1.2.3",
		RegistrationToken: "tok",
		Signature:         []byte{1, 2, 3},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var gw gatewayHelloMsg
	if err := json.Unmarshal(data, &gw); err != nil {
		t.Fatalf("unmarshal into gateway-literal shape: %v", err)
	}
	if gw.Type != "hello" || gw.ConnectorID != "c1" || gw.ProtocolVersion != "v1" || gw.ConnectorVersion != "1.2.3" {
		t.Errorf("gateway-literal decode = %+v, want matching scalar fields", gw)
	}
	if gw.RegistrationToken != "tok" {
		t.Errorf("registration_token = %q, want %q (stale 'token' field name?)", gw.RegistrationToken, "tok")
	}
	sigBytes, err := base64.StdEncoding.DecodeString(gw.Signature)
	if err != nil {
		t.Fatalf("signature is not valid base64 (gateway does base64.StdEncoding.DecodeString on it): %v", err)
	}
	if !bytes.Equal(sigBytes, []byte{1, 2, 3}) {
		t.Errorf("decoded signature = %v, want %v", sigBytes, []byte{1, 2, 3})
	}
}

// TestChallengeMsgUnmarshalsGatewayFormat verifies mkonnect can READ the exact frame the
// gateway sends: {"type":"challenge","challenge":"<base64>"} — not "nonce". The JSON here
// is hand-written, not produced by proto.ChallengeMsg's own marshaler, so a bug in
// Nonce.UnmarshalJSON can't be masked by a matching bug in MarshalJSON.
func TestChallengeMsgUnmarshalsGatewayFormat(t *testing.T) {
	var nonce proto.Nonce
	for i := range nonce {
		nonce[i] = byte(i)
	}
	raw := fmt.Sprintf(`{"type":"challenge","challenge":%q}`, base64.StdEncoding.EncodeToString(nonce[:]))

	var msg proto.ChallengeMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "challenge" {
		t.Errorf("type = %q, want %q", msg.Type, "challenge")
	}
	if msg.Challenge != nonce {
		t.Errorf("challenge = %v, want %v", msg.Challenge, nonce)
	}
}

// TestHandshakeErrorMsgUnmarshalsGatewayFormat verifies mkonnect reads the gateway's actual
// error shape: {"type":"handshake_error","code":<int>,"reason":"<string>"} — not the old
// {"type":"error","code":"<string>","message":"<string>"}. code is a JSON number here, so
// this would fail to unmarshal against a string-typed Code field.
func TestHandshakeErrorMsgUnmarshalsGatewayFormat(t *testing.T) {
	raw := `{"type":"handshake_error","code":4001,"reason":"bad token"}`
	var msg proto.HandshakeErrorMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Code != 4001 {
		t.Errorf("code = %d, want 4001", msg.Code)
	}
	if msg.Reason != "bad token" {
		t.Errorf("reason = %q, want %q", msg.Reason, "bad token")
	}
}

// TestHandshakeOkMsgUnmarshalsGatewayFormat verifies mkonnect reads deprecation_notice,
// critical_patch_required (ADR-063 §10, both previously ignored entirely), and a base64
// private_key string from the gateway's actual handshake_ok shape.
func TestHandshakeOkMsgUnmarshalsGatewayFormat(t *testing.T) {
	raw := `{"type":"handshake_ok","deprecation_notice":"upgrade by 2026-12-01","critical_patch_required":true,"private_key":"AQIDBA=="}`
	var msg proto.HandshakeOkMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.DeprecationNotice != "upgrade by 2026-12-01" {
		t.Errorf("DeprecationNotice = %q, want %q", msg.DeprecationNotice, "upgrade by 2026-12-01")
	}
	if !msg.CriticalPatch {
		t.Error("CriticalPatch = false, want true")
	}
	if !bytes.Equal(msg.PrivateKey, []byte{1, 2, 3, 4}) {
		t.Errorf("PrivateKey = %v, want %v", msg.PrivateKey, []byte{1, 2, 3, 4})
	}
}

// TestHealthMsgType verifies the heartbeat message mkonnect writes serializes with
// type "health" — the gateway's router.go only updates last_health_at from this exact type.
func TestHealthMsgType(t *testing.T) {
	data, err := json.Marshal(proto.HealthMsg{Type: "health"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "health" {
		t.Errorf("type = %v, want %q", m["type"], "health")
	}
}

func TestHTTPRequestMsgRoundtrip(t *testing.T) {
	body := `{"q":1}`
	orig := proto.HTTPRequestMsg{
		Type:        "http_request",
		RequestID:   "req-1",
		ResponderID: "replica-uuid-42",
		ProviderKey: "jenkins",
		Method:      "GET",
		Path:        "/api/json",
		Headers:     map[string]string{"X-Foo": "bar"},
		Body:        &body,
	}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got proto.HTTPRequestMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProviderKey != "jenkins" || got.ResponderID != "replica-uuid-42" || got.Headers["X-Foo"] != "bar" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestConnectionStatusMsgRoundtrip(t *testing.T) {
	orig := proto.ConnectionStatusMsg{
		Type: "connection_status",
		Plugins: []proto.PluginStatus{
			{ProviderKey: "jenkins", Status: "configured_connected", LastTestedAt: "2026-09-14T00:00:00Z"},
			{ProviderKey: "prom", Status: "not_configured"},
		},
	}
	b, _ := json.Marshal(orig)
	var got proto.ConnectionStatusMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Plugins) != 2 || got.Plugins[0].Status != "configured_connected" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestHTTPRequestMsgExpiresAtRoundtrip checks the optional expires_at field survives a
// marshal/unmarshal cycle when set.
func TestHTTPRequestMsgExpiresAtRoundtrip(t *testing.T) {
	orig := proto.HTTPRequestMsg{
		Type:        "http_request",
		RequestID:   "req-1",
		ProviderKey: "svc",
		Method:      "GET",
		Path:        "/api",
		ExpiresAt:   "2026-10-20T12:00:00Z",
	}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"expires_at":"2026-10-20T12:00:00Z"`)) {
		t.Fatalf("expires_at not present in JSON: %s", b)
	}
	var got proto.HTTPRequestMsg
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExpiresAt != orig.ExpiresAt {
		t.Fatalf("ExpiresAt = %q, want %q", got.ExpiresAt, orig.ExpiresAt)
	}
}

// TestHTTPRequestMsgExpiresAtOmittedWhenEmpty checks that an unset expires_at is left off
// the wire (omitempty), so the encoding stays byte-identical to a message without the field.
func TestHTTPRequestMsgExpiresAtOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(proto.HTTPRequestMsg{Type: "http_request", RequestID: "req-1", Path: "/"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("expires_at")) {
		t.Fatalf("empty ExpiresAt must be omitted from JSON, got: %s", b)
	}
}

// TestHTTPRequestMsgOldFrameWithoutExpiresAt checks that a message produced by an older
// platform — a raw frame with no expires_at field — still decodes, leaving ExpiresAt empty.
func TestHTTPRequestMsgOldFrameWithoutExpiresAt(t *testing.T) {
	raw := `{"type":"http_request","request_id":"req-1","responder_id":"r-1","provider_key":"svc","method":"GET","path":"/api","body":null}`
	var got proto.HTTPRequestMsg
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal old frame: %v", err)
	}
	if got.ExpiresAt != "" {
		t.Fatalf("ExpiresAt = %q, want empty for a frame without the field", got.ExpiresAt)
	}
	if got.ProviderKey != "svc" || got.Path != "/api" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestHTTPRequestMsgNumericExpiresAtRejected documents the existing frame contract: like
// every other field, expires_at is typed (string), so a non-string value (e.g. an epoch
// number) fails to decode the whole frame rather than being coerced. The format-level
// fail-open on a parseable-but-malformed string (handled in the plugin handler) does NOT
// extend to a type mismatch at the JSON layer — the platform must send a string. This pins
// current behaviour so a future change to it is deliberate, not accidental.
func TestHTTPRequestMsgNumericExpiresAtRejected(t *testing.T) {
	raw := `{"type":"http_request","provider_key":"svc","path":"/","expires_at":1700000000}`
	var got proto.HTTPRequestMsg
	if err := json.Unmarshal([]byte(raw), &got); err == nil {
		t.Fatalf("expected unmarshal error for numeric expires_at, got none: %+v", got)
	}
}
