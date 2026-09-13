package auth

import (
	"fmt"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

// privKeySize is the packed size of an ML-DSA-65 private key.
const privKeySize = mldsa65.PrivateKeySize

// Sign signs challenge||connectorID using the ML-DSA-65 private key.
// The message is challenge (32 bytes) concatenated with the connectorID string.
// It returns the signature bytes.
func Sign(priv *mldsa65.PrivateKey, challenge [32]byte, connectorID string) []byte {
	msg := make([]byte, 32+len(connectorID))
	copy(msg[:32], challenge[:])
	copy(msg[32:], connectorID)

	sig := make([]byte, mldsa65.SignatureSize)
	if err := mldsa65.SignTo(priv, msg, nil, false, sig); err != nil {
		// SignTo only fails if sig buffer is wrong size; panic is appropriate here.
		panic(fmt.Sprintf("auth.Sign: unexpected SignTo error: %v", err))
	}
	return sig
}

// UnmarshalPrivateKey deserializes a packed ML-DSA-65 private key from bytes.
func UnmarshalPrivateKey(data []byte) (*mldsa65.PrivateKey, error) {
	if len(data) != mldsa65.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key size: got %d, want %d", len(data), mldsa65.PrivateKeySize)
	}
	var buf [mldsa65.PrivateKeySize]byte
	copy(buf[:], data)
	var priv mldsa65.PrivateKey
	priv.Unpack(&buf)
	return &priv, nil
}
