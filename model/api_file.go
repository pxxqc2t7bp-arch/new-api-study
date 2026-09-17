package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	APIFilePurposeBatch       = "batch"
	APIFilePurposeBatchOutput = "batch_output"
	APIFilePurposeBatchError  = "batch_error"

	APIFileStatusProcessed = "processed"
)

type APIFile struct {
	ID         int64  `json:"-" gorm:"primaryKey"`
	FileID     string `json:"id" gorm:"type:varchar(64);uniqueIndex"`
	Object     string `json:"object" gorm:"type:varchar(16);not null"`
	UserID     int    `json:"-" gorm:"index"`
	TokenID    int    `json:"-" gorm:"index"`
	Purpose    string `json:"purpose" gorm:"type:varchar(32);index"`
	Filename   string `json:"filename" gorm:"type:varchar(255)"`
	Bytes      int64  `json:"bytes" gorm:"bigint"`
	SHA256     string `json:"-" gorm:"type:char(64)"`
	StorageKey string `json:"-" gorm:"type:varchar(255);uniqueIndex"`
	Status     string `json:"status" gorm:"type:varchar(32);index"`
	CreatedAt  int64  `json:"created_at" gorm:"bigint;index"`
	ExpiresAt  int64  `json:"expires_at,omitempty" gorm:"bigint;index"`
}

type APIFileDeleted struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

func (file *APIFile) BeforeCreate(_ *gorm.DB) error {
	if file.FileID == "" {
		value, err := GenerateAPIFileID()
		if err != nil {
			return err
		}
		file.FileID = value
	}
	if file.Object == "" {
		file.Object = "file"
	}
	if file.Status == "" {
		file.Status = APIFileStatusProcessed
	}
	if file.CreatedAt == 0 {
		file.CreatedAt = time.Now().Unix()
	}
	return nil
}

func GenerateAPIFileID() (string, error) {
	value, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return "", err
	}
	return "file-" + value, nil
}

func CreateAPIFile(file *APIFile) error {
	if file == nil {
		return errors.New("file is required")
	}
	return DB.Create(file).Error
}

func GetOwnedAPIFile(userID, tokenID int, fileID string) (*APIFile, bool, error) {
	if userID <= 0 || tokenID <= 0 || fileID == "" {
		return nil, false, nil
	}
	var file APIFile
	err := DB.Where("user_id = ? AND token_id = ? AND file_id = ?", userID, tokenID, fileID).
		Where("expires_at = 0 OR expires_at > ?", time.Now().Unix()).
		First(&file).Error
	exists, err := RecordExist(err)
	if err != nil || !exists {
		return nil, exists, err
	}
	return &file, true, nil
}

func ListOwnedAPIFiles(userID, tokenID int, purpose, after string, limit int) ([]APIFile, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := DB.Where("user_id = ? AND token_id = ?", userID, tokenID)
	query = query.Where("expires_at = 0 OR expires_at > ?", time.Now().Unix())
	if purpose != "" {
		query = query.Where("purpose = ?", purpose)
	}
	if after != "" {
		var cursor APIFile
		if err := DB.Select("id").Where(
			"user_id = ? AND token_id = ? AND file_id = ?",
			userID,
			tokenID,
			after,
		).First(&cursor).Error; err != nil {
			return nil, false, err
		}
		query = query.Where("id < ?", cursor.ID)
	}
	var files []APIFile
	if err := query.Order("id desc").Limit(limit + 1).Find(&files).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(files) > limit
	if hasMore {
		files = files[:limit]
	}
	return files, hasMore, nil
}

func DeleteOwnedAPIFile(userID, tokenID int, fileID string) (*APIFile, bool, error) {
	file, exists, err := GetOwnedAPIFile(userID, tokenID, fileID)
	if err != nil || !exists {
		return file, exists, err
	}
	var activeReferences int64
	if err = DB.Model(&Batch{}).
		Where("input_file_id = ? AND status IN ?", fileID, activeBatchStatuses()).
		Count(&activeReferences).Error; err != nil {
		return nil, false, err
	}
	if activeReferences > 0 {
		return file, true, errors.New("file is used by an active batch")
	}
	if err = DB.Delete(file).Error; err != nil {
		return nil, true, err
	}
	return file, true, nil
}

func ListExpiredAPIFiles(now int64, limit int) ([]*APIFile, error) {
	if limit <= 0 {
		limit = 100
	}
	var files []*APIFile
	err := DB.Where("expires_at > 0 AND expires_at <= ?", now).
		Order("id").
		Limit(limit).
		Find(&files).Error
	return files, err
}

func DeleteExpiredAPIFile(file *APIFile, now int64) (bool, error) {
	if file == nil || file.ID == 0 || file.ExpiresAt == 0 || file.ExpiresAt > now {
		return false, nil
	}
	var activeReferences int64
	if err := DB.Model(&Batch{}).
		Where("input_file_id = ? AND status IN ?", file.FileID, activeBatchStatuses()).
		Count(&activeReferences).Error; err != nil {
		return false, err
	}
	if activeReferences > 0 {
		return false, nil
	}
	result := DB.Where("id = ? AND expires_at > 0 AND expires_at <= ?", file.ID, now).Delete(&APIFile{})
	return result.RowsAffected > 0, result.Error
}
