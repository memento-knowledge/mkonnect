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
// On first run, Token is set and Signature is nil.
// On reconnect, Signature is set and Token is empty.
type HelloMsg struct {
	Type            string `json:"type"`
	ConnectorID     string `json:"connector_id"`
	ProtocolVersion string `json:"protocol_version"`
	Token           string `json:"token,omitempty"`
	Signature       []byte `json:"signature,omitempty"`
}

// HandshakeOkMsg is sent by the gateway after successful first-run registration.
// PrivateKey contains the ML-DSA-65 private key bytes to persist locally.
type HandshakeOkMsg struct {
	Type       string `json:"type"`
	PrivateKey []byte `json:"private_key,omitempty"`
}

// ChallengeMsg is sent by the gateway on reconnect to authenticate the connector.
type ChallengeMsg struct {
	Type  string `json:"type"`
	Nonce Nonce  `json:"nonce"`
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

// ErrorMsg is sent by the gateway to signal a protocol error.
type ErrorMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
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
