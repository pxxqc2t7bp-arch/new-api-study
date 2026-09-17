package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

func TestVolcEngineArkMediaEndpointTypes(t *testing.T) {
	tests := []struct {
		model string
		want  []constant.EndpointType
	}{
		{
			model: "doubao-seedream-5-0-260128",
			want:  []constant.EndpointType{constant.EndpointTypeImageGeneration},
		},
		{
			model: "stable-diffusion-xl-v1",
			want:  []constant.EndpointType{constant.EndpointTypeImageGeneration},
		},
		{
			model: "doubao-seedance-2-5-260628",
			want:  []constant.EndpointType{constant.EndpointTypeOpenAIVideo},
		},
		{
			model: "glm-5-3-260814",
			want:  []constant.EndpointType{constant.EndpointTypeOpenAI},
		},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			assert.Equal(
				t,
				test.want,
				GetEndpointTypesByChannelType(
					constant.ChannelTypeVolcEngine,
					test.model,
				),
			)
		})
	}
}

func TestArkMediaModelClassification(t *testing.T) {
	assert.True(t, IsImageGenerationModel("doubao-seedream-4-5-251128"))
	assert.True(t, IsImageGenerationModel("stable-diffusion-xl-v1"))
	assert.False(t, IsImageGenerationModel("doubao-seedance-2-5-260628"))
	assert.True(t, IsVideoGenerationModel("doubao-seedance-2-5-260628"))
	assert.False(t, IsVideoGenerationModel("glm-5-3-260814"))
}
