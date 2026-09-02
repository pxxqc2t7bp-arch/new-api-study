package controller

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
)

func TestUpstreamDailyHandlerDue(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	handler := upstreamDailyHandler{}

	assert.False(t, handler.Due(time.Date(2026, 9, 2, 2, 59, 0, 0, location), nil))
	assert.True(t, handler.Due(time.Date(2026, 9, 2, 3, 0, 0, 0, location), nil))

	latest := &model.SystemTask{UpdatedAt: time.Date(2026, 9, 2, 3, 1, 0, 0, location).Unix()}
	assert.False(t, handler.Due(time.Date(2026, 9, 2, 8, 0, 0, 0, location), latest))
	assert.True(t, handler.Due(time.Date(2026, 9, 3, 3, 0, 0, 0, location), latest))
}
