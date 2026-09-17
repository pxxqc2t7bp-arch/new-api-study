package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

const (
	upstreamPairingTTL    = 10 * time.Minute
	upstreamDevicePrefix  = "uos_"
	upstreamPairingPrefix = "uop_"
)

var (
	ErrUpstreamDeviceUnauthorized = errors.New("upstream sync device is unauthorized")
	ErrUpstreamPairingInvalid     = errors.New("upstream sync pairing code is invalid or expired")
)

func upstreamDeviceTokenHash(token string) string {
	return common.GenerateHMACWithKey(
		[]byte("upstream-sync-device-v1:"+common.SessionSecret),
		strings.TrimSpace(token),
	)
}

func CreateUpstreamPairingCode(deviceName string) (*model.UpstreamSyncDevice, string, error) {
	deviceSecret, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return nil, "", err
	}
	pairingSecret, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return nil, "", err
	}
	now := common.GetTimestamp()
	pairingHash := upstreamDeviceTokenHash(upstreamPairingPrefix + pairingSecret)
	device := &model.UpstreamSyncDevice{
		DeviceID:         "uosd_" + deviceSecret,
		Name:             strings.TrimSpace(deviceName),
		PairingHash:      &pairingHash,
		PairingExpiresAt: now + int64(upstreamPairingTTL.Seconds()),
		Status:           model.UpstreamSyncDevicePending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if device.Name == "" {
		device.Name = "Chrome"
	}
	if err := model.DB.Create(device).Error; err != nil {
		return nil, "", err
	}
	return device, upstreamPairingPrefix + pairingSecret, nil
}

func PairUpstreamSyncDevice(pairingCode string, deviceName string) (*model.UpstreamSyncDevice, string, error) {
	pairingCode = strings.TrimSpace(pairingCode)
	if !strings.HasPrefix(pairingCode, upstreamPairingPrefix) {
		return nil, "", ErrUpstreamPairingInvalid
	}
	now := common.GetTimestamp()
	var device model.UpstreamSyncDevice
	err := model.DB.Where(
		"pairing_hash = ? AND status = ? AND pairing_expires_at > ?",
		upstreamDeviceTokenHash(pairingCode),
		model.UpstreamSyncDevicePending,
		now,
	).First(&device).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", ErrUpstreamPairingInvalid
		}
		return nil, "", err
	}

	tokenSecret, err := common.GenerateRandomCharsKey(48)
	if err != nil {
		return nil, "", err
	}
	token := upstreamDevicePrefix + tokenSecret
	tokenHash := upstreamDeviceTokenHash(token)
	name := strings.TrimSpace(deviceName)
	if name == "" {
		name = device.Name
	}
	result := model.DB.Model(&model.UpstreamSyncDevice{}).
		Where("id = ? AND status = ? AND pairing_hash = ?", device.ID, model.UpstreamSyncDevicePending, upstreamDeviceTokenHash(pairingCode)).
		Updates(map[string]any{
			"name":               name,
			"token_hash":         tokenHash,
			"pairing_hash":       nil,
			"pairing_expires_at": int64(0),
			"status":             model.UpstreamSyncDeviceActive,
			"last_seen_at":       now,
			"updated_at":         now,
		})
	if result.Error != nil {
		return nil, "", result.Error
	}
	if result.RowsAffected != 1 {
		return nil, "", ErrUpstreamPairingInvalid
	}
	device.Name = name
	device.TokenHash = &tokenHash
	device.PairingHash = nil
	device.PairingExpiresAt = 0
	device.Status = model.UpstreamSyncDeviceActive
	device.LastSeenAt = now
	device.UpdatedAt = now
	return &device, token, nil
}

func AuthenticateUpstreamSyncDevice(token string) (*model.UpstreamSyncDevice, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, upstreamDevicePrefix) {
		return nil, ErrUpstreamDeviceUnauthorized
	}
	var device model.UpstreamSyncDevice
	if err := model.DB.Where(
		"token_hash = ? AND status = ?",
		upstreamDeviceTokenHash(token),
		model.UpstreamSyncDeviceActive,
	).First(&device).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUpstreamDeviceUnauthorized
		}
		return nil, err
	}
	now := common.GetTimestamp()
	if err := model.DB.Model(&device).Updates(map[string]any{
		"last_seen_at": now,
		"updated_at":   now,
	}).Error; err != nil {
		return nil, fmt.Errorf("touch upstream sync device: %w", err)
	}
	device.LastSeenAt = now
	device.UpdatedAt = now
	return &device, nil
}

func RevokeUpstreamSyncDevice(deviceID string) error {
	now := common.GetTimestamp()
	result := model.DB.Model(&model.UpstreamSyncDevice{}).
		Where("device_id = ?", strings.TrimSpace(deviceID)).
		Updates(map[string]any{
			"status":             model.UpstreamSyncDeviceRevoked,
			"token_hash":         nil,
			"pairing_hash":       nil,
			"pairing_expires_at": int64(0),
			"revoked_at":         now,
			"updated_at":         now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func ListUpstreamSyncDevices() ([]model.UpstreamSyncDevice, error) {
	var devices []model.UpstreamSyncDevice
	err := model.DB.Order("id desc").Find(&devices).Error
	return devices, err
}
