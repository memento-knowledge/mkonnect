// Package version holds mkonnect's build version, reported to the Bridge Gateway in the
// handshake (proto.HelloMsg.ConnectorVersion) and shown in the portal's connector status.
package version

// Version identifies this build. It defaults to "dev" for local/unreleased builds and is
// overridden at build time via:
//
//	-ldflags "-X github.com/memento-knowledge/mkonnect/internal/version.Version=<value>"
//
// See Dockerfile and .github/workflows/ci.yml.
var Version = "dev"
