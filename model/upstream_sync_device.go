package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	UpstreamSyncDevicePending = "pending"
	UpstreamSyncDeviceActive  = "active"
	UpstreamSyncDeviceRevoked = "revoked"

	UpstreamSyncCommandPending   = "pending"
	UpstreamSyncCommandRunning   = "running"
	UpstreamSyncCommandSucceeded = "succeeded"
	UpstreamSyncCommandFailed    = "failed"
)

type UpstreamSyncDevice struct {
	ID               int64   `json:"id" gorm:"primaryKey"`
	DeviceID         string  `json:"device_id" gorm:"type:varchar(64);not null;uniqueIndex"`
	Name             string  `json:"name" gorm:"type:varchar(128);not null"`
	TokenHash        *string `json:"-" gorm:"type:char(64);uniqueIndex"`
	PairingHash      *string `json:"-" gorm:"type:char(64);uniqueIndex"`
	PairingExpiresAt int64   `json:"pairing_expires_at,omitempty" gorm:"bigint;index"`
	Status           string  `json:"status" gorm:"type:varchar(16);not null;index"`
	LastSeenAt       int64   `json:"last_seen_at,omitempty" gorm:"bigint;index"`
	CreatedAt        int64   `json:"created_at" gorm:"bigint;index"`
	UpdatedAt        int64   `json:"updated_at" gorm:"bigint;index"`
	RevokedAt        int64   `json:"revoked_at,omitempty" gorm:"bigint;index"`
}

func (UpstreamSyncDevice) TableName() string {
	return "upstream_sync_devices"
}

func (device *UpstreamSyncDevice) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if device.Status == "" {
		device.Status = UpstreamSyncDevicePending
	}
	if device.CreatedAt == 0 {
		device.CreatedAt = now
	}
	if device.UpdatedAt == 0 {
		device.UpdatedAt = now
	}
	return nil
}

type UpstreamSyncBatch struct {
	ID          int64  `json:"id" gorm:"primaryKey"`
	SnapshotID  string `json:"snapshot_id" gorm:"type:varchar(64);not null;uniqueIndex"`
	DeviceID    string `json:"device_id" gorm:"type:varchar(64);not null;index"`
	CapturedAt  int64  `json:"captured_at" gorm:"bigint;not null;index"`
	SourceCount int    `json:"source_count" gorm:"not null"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint;index"`
}

func (UpstreamSyncBatch) TableName() string {
	return "upstream_sync_batches"
}

func (batch *UpstreamSyncBatch) BeforeCreate(_ *gorm.DB) error {
	if batch.CreatedAt == 0 {
		batch.CreatedAt = common.GetTimestamp()
	}
	return nil
}

type UpstreamSyncCommand struct {
	ID          int64  `json:"id" gorm:"primaryKey"`
	CommandID   string `json:"command_id" gorm:"type:varchar(64);not null;uniqueIndex"`
	DeviceID    string `json:"device_id,omitempty" gorm:"type:varchar(64);index"`
	Type        string `json:"type" gorm:"type:varchar(32);not null;index"`
	SourceKey   string `json:"source_key,omitempty" gorm:"type:varchar(32);index"`
	Payload     string `json:"payload" gorm:"type:text"`
	Status      string `json:"status" gorm:"type:varchar(16);not null;index"`
	Result      string `json:"result" gorm:"type:text"`
	Error       string `json:"error,omitempty" gorm:"type:text"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint;index"`
	UpdatedAt   int64  `json:"updated_at" gorm:"bigint;index"`
	CompletedAt int64  `json:"completed_at,omitempty" gorm:"bigint"`
}

func (UpstreamSyncCommand) TableName() string {
	return "upstream_sync_commands"
}

func (command *UpstreamSyncCommand) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if command.Status == "" {
		command.Status = UpstreamSyncCommandPending
	}
	if command.CreatedAt == 0 {
		command.CreatedAt = now
	}
	if command.UpdatedAt == 0 {
		command.UpdatedAt = now
	}
	return nil
}

func ClaimPendingUpstreamSyncCommands(deviceID string, limit int) ([]UpstreamSyncCommand, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var commands []UpstreamSyncCommand
	err := DB.Transaction(func(tx *gorm.DB) error {
		now := common.GetTimestamp()
		if err := tx.Model(&UpstreamSyncCommand{}).
			Where("status = ? AND updated_at < ?", UpstreamSyncCommandRunning, now-600).
			Updates(map[string]any{
				"status":     UpstreamSyncCommandPending,
				"device_id":  "",
				"updated_at": now,
			}).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where(
			"status = ? AND (device_id = ? OR device_id = '')",
			UpstreamSyncCommandPending,
			deviceID,
		).Order("id asc").Limit(limit).Find(&commands).Error; err != nil {
			return err
		}
		if len(commands) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(commands))
		for _, command := range commands {
			ids = append(ids, command.ID)
		}
		result := tx.Model(&UpstreamSyncCommand{}).
			Where("id IN ? AND status = ?", ids, UpstreamSyncCommandPending).
			Updates(map[string]any{"device_id": deviceID, "status": UpstreamSyncCommandRunning, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(ids)) {
			return errors.New("upstream sync command claim conflict")
		}
		for i := range commands {
			commands[i].DeviceID = deviceID
			commands[i].Status = UpstreamSyncCommandRunning
			commands[i].UpdatedAt = now
		}
		return nil
	})
	return commands, err
}

func CreateUpstreamSyncCommand(deviceID string, commandType string, sourceKey string, payload any) (*UpstreamSyncCommand, error) {
	commandSecret, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return nil, err
	}
	payloadText := ""
	if payload != nil {
		data, marshalErr := common.Marshal(payload)
		if marshalErr != nil {
			return nil, marshalErr
		}
		payloadText = string(data)
	}
	command := &UpstreamSyncCommand{
		CommandID: "uosc_" + commandSecret,
		DeviceID:  deviceID,
		Type:      commandType,
		SourceKey: sourceKey,
		Payload:   payloadText,
		Status:    UpstreamSyncCommandPending,
	}
	if err := DB.Create(command).Error; err != nil {
		return nil, err
	}
	return command, nil
}

func CompleteUpstreamSyncCommand(commandID string, status string, result string, errorMessage string) error {
	if status != UpstreamSyncCommandSucceeded && status != UpstreamSyncCommandFailed {
		return errors.New("invalid upstream sync command status")
	}
	now := common.GetTimestamp()
	res := DB.Model(&UpstreamSyncCommand{}).
		Where("command_id = ? AND status = ?", commandID, UpstreamSyncCommandRunning).
		Updates(map[string]any{
			"status":       status,
			"result":       result,
			"error":        errorMessage,
			"updated_at":   now,
			"completed_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func UpsertUpstreamSyncDevice(device *UpstreamSyncDevice) error {
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "device_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name",
			"token_hash",
			"pairing_hash",
			"pairing_expires_at",
			"status",
			"last_seen_at",
			"updated_at",
			"revoked_at",
		}),
	}).Create(device).Error
}

func ListUpstreamSyncCommands(limit int) ([]UpstreamSyncCommand, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var commands []UpstreamSyncCommand
	err := DB.Order("id desc").Limit(limit).Find(&commands).Error
	return commands, err
}

func GetUpstreamSyncCommand(commandID string) (*UpstreamSyncCommand, error) {
	var command UpstreamSyncCommand
	if err := DB.Where("command_id = ?", commandID).First(&command).Error; err != nil {
		return nil, err
	}
	return &command, nil
}
