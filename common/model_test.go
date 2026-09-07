package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsImageGenerationModelRecognizesSeedream(t *testing.T) {
	assert.True(t, IsImageGenerationModel("doubao-seedream-5-0-pro-260628"))
	assert.True(t, IsImageGenerationModel("doubao-seedream-4-5-251128"))
	assert.False(t, IsImageGenerationModel("doubao-seedance-2-5-260628"))
}
