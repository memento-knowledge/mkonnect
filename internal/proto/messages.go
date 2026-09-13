// Package proto defines the JSON message types for the mkonnect <-> Bridge Gateway WebSocket protocol.
package proto

import (
	"encoding/json"
	"fmt"
)

// TypedMessage is used to inspect the Type field before full unmarshalling.
type TypedMessage struct {
	Type string `json:"type"`
}

// HelloMsg is sent by the connector to the gateway to initiate a session.
// On first run, RegistrationToken is set and Signature is nil.
// On reconnect, Signature is set and RegistrationToken is empty.
type HelloMsg struct {
	Type              string `json:"type"`
	ConnectorID       string `json:"connector_id"`
	ProtocolVersion   string `json:"protocol_version"`
	ConnectorVersion  string `json:"connector_version"`
	RegistrationToken string `json:"registration_token,omitempty"`
	Signature         []byte `json:"signature,omitempty"`
}

// HandshakeOkMsg is sent by the gateway after a successful handshake (registration or reconnect).
// PrivateKey is set only on first-run registration. DeprecationNotice and CriticalPatch implement
// the protocol_version compatibility window (ADR-063 §10) — a non-empty DeprecationNotice means
// the connector's protocol_version is still accepted but scheduled for removal; CriticalPatch
// means the gateway wants new tool dispatch suspended until the connector is updated.
type HandshakeOkMsg struct {
	Type              string `json:"type"`
	DeprecationNotice string `json:"deprecation_notice,omitempty"`
	CriticalPatch     bool   `json:"critical_patch_required,omitempty"`
	PrivateKey        []byte `json:"private_key,omitempty"`
}

// ChallengeMsg is sent by the gateway, unconditionally, as the first message on every
// connection attempt — both first-run registration (which ignores it) and reconnect
// (which signs it) receive it before sending HelloMsg.
type ChallengeMsg struct {
	Type      string `json:"type"`
	Challenge Nonce  `json:"challenge"`
}

// Nonce wraps a 32-byte array that serialises as base64 JSON.
type Nonce [32]byte

func (n Nonce) MarshalJSON() ([]byte, error) {
	return json.Marshal(n[:])
}

func (n *Nonce) UnmarshalJSON(data []byte) error {
	var b []byte
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	if len(b) != 32 {
		return fmt.Errorf("proto: nonce must be 32 bytes, got %d", len(b))
	}
	copy(n[:], b)
	return nil
}

// HandshakeErrorMsg is sent by the gateway to reject a handshake (registration or reconnect).
type HandshakeErrorMsg struct {
	Type   string `json:"type"` // "handshake_error"
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// HealthMsg is an application-level heartbeat the connector sends every 30 seconds.
// The gateway's post-auth message loop only updates its liveness tracking from this
// message type — a transport-level WebSocket ping/pong frame is invisible to it.
type HealthMsg struct {
	Type string `json:"type"` // "health"
}

// DataMsg is an inbound request from the platform delivered via the gateway.
type DataMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Plugin    string `json:"plugin"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Body      []byte `json:"body,omitempty"`
}

// ResponseMsg is the outbound response to the platform, sent via the gateway.
type ResponseMsg struct {
	Type       string `json:"type"`
	RequestID  string `json:"request_id"`
	StatusCode int    `json:"status_code"`
	Body       []byte `json:"body,omitempty"`
}
