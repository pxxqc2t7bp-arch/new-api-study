package model

import (
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

func usageQueryForViewer(query *gorm.DB, viewerRole int, userIDColumn string) (*gorm.DB, error) {
	if viewerRole != common.RoleAdminUser {
		return query, nil
	}

	var protectedUserIDs []int
	if err := DB.Unscoped().
		Model(&User{}).
		Where("role = ?", common.RolePluginAdminUser).
		Pluck("id", &protectedUserIDs).Error; err != nil {
		return nil, err
	}
	if len(protectedUserIDs) == 0 {
		return query, nil
	}
	return query.Where(userIDColumn+" NOT IN ?", protectedUserIDs), nil
}
