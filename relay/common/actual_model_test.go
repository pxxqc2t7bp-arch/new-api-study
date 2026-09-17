package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCaptureActualUpstreamModelName(t *testing.T) {
	info := &RelayInfo{OriginModelName: "doubao-smart-router-250928"}

	info.CaptureActualUpstreamModelName("doubao-seed-2-0-mini-260428")
	info.CaptureActualUpstreamModelName("doubao-seed-2-0-mini-260428")

	assert.Equal(t, "doubao-seed-2-0-mini-260428", info.ActualUpstreamModelName)
	assert.False(t, info.ActualModelConflict)

	info.CaptureActualUpstreamModelName("doubao-seed-2-1-pro-260628")
	assert.True(t, info.ActualModelConflict)
}

func TestCaptureActualUpstreamModelNameIgnoresOrdinaryModels(t *testing.T) {
	info := &RelayInfo{OriginModelName: "glm-5.3"}
	info.CaptureActualUpstreamModelName("other")
	assert.Empty(t, info.ActualUpstreamModelName)
}
