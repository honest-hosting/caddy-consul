package caddyconsul

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsConnect(t *testing.T) {
	assert.True(t, UpstreamConnectSidecar.IsConnect())
	assert.False(t, UpstreamDirect.IsConnect())
	assert.False(t, UpstreamMode("").IsConnect())
	assert.False(t, UpstreamMode("connect").IsConnect())
}
