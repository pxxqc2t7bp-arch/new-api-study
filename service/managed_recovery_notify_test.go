package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
)

func TestNotifyManagedChannelRecoveredMatchesDirectEnableNotification(t *testing.T) {
	var notificationType string
	var subject string
	var content string
	notify := func(gotType string, gotSubject string, gotContent string) {
		notificationType = gotType
		subject = gotSubject
		content = gotContent
	}

	channel := &model.Channel{Id: 73, Name: "managed recovered"}
	notifyManagedChannelRecovered(channel, notify)

	assert.Equal(t, formatNotifyType(channel.Id, common.ChannelStatusEnabled), notificationType)
	assert.Equal(t, "通道「managed recovered」（#73）已被启用", subject)
	assert.Equal(t, subject, content)
	assert.Contains(t, notificationType, dto.NotifyTypeChannelUpdate)
}
