//go:build !linux
// +build !linux

package agentutils

// MasqueradeSelf is Linux-only (memfd + fexecve); on other platforms the
// agent keeps its stock identity. See masquerade_linux.go.
func MasqueradeSelf() {}
