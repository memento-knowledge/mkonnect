//go:build tools

package tools

import (
	_ "github.com/cloudflare/circl/sign/mldsa/mldsa65"
	_ "github.com/coder/websocket"
	_ "github.com/golang-jwt/jwt/v5"
)
