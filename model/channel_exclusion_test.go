package model

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
)

func TestChannelSatisfiesExcludeChannelIDsFilter(t *testing.T) {
	channel := &Channel{Id: 62}
	filter := dto.ChannelFilter{
		Kind:               dto.FilterExcludeChannelIDs,
		ExcludedChannelIDs: []int{54, 62},
	}

	ok, kind := ChannelSatisfiesFilters(channel, "gpt-5.4", []dto.ChannelFilter{filter})

	assert.False(t, ok)
	assert.Equal(t, dto.FilterExcludeChannelIDs, kind)

	filter.ExcludedChannelIDs = []int{54}
	ok, kind = ChannelSatisfiesFilters(channel, "gpt-5.4", []dto.ChannelFilter{filter})
	assert.True(t, ok)
	assert.Empty(t, kind)
}
