package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageRequestBillingUsage(t *testing.T) {
	n := uint(3)
	request := ImageRequest{
		Model:  "doubao-seedream-5-0-pro-260628",
		N:      &n,
		Size:   "2048x2048",
		Image:  json.RawMessage(`"https://example.com/one.png"`),
		Images: json.RawMessage(`["https://example.com/two.png","https://example.com/three.png"]`),
	}

	meta := request.GetTokenCountMeta()
	require.NotNil(t, meta)
	assert.Equal(t, 3.0, meta.BillingUsage["image_count"])
	assert.Equal(t, "2K", meta.BillingUsage["resolution"])
	assert.Equal(t, 3.0, meta.BillingUsage["reference_image_count"])
}

func TestImageRequestBillingUsageUsesModelDefaultResolution(t *testing.T) {
	request := ImageRequest{Model: "doubao-seedream-5-0-260128"}

	meta := request.GetTokenCountMeta()
	require.NotNil(t, meta)
	assert.Equal(t, 1.0, meta.BillingUsage["image_count"])
	assert.Equal(t, "2K", meta.BillingUsage["resolution"])
	assert.Equal(t, 0.0, meta.BillingUsage["reference_image_count"])
}
