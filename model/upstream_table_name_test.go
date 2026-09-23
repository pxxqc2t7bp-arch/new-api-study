package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestUpstreamManagedModelsHonorNamingStrategy(t *testing.T) {
	models := []struct {
		name      string
		value     any
		tableName string
	}{
		{name: "source", value: &UpstreamSource{}, tableName: "upstream_sources"},
		{name: "group", value: &UpstreamGroup{}, tableName: "upstream_groups"},
		{name: "route", value: &UpstreamManagedRoute{}, tableName: "upstream_managed_routes"},
	}
	strategies := []struct {
		name   string
		prefix string
	}{
		{name: "default"},
		{name: "prefixed", prefix: "isolated_"},
	}

	for _, strategy := range strategies {
		t.Run(strategy.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
				NamingStrategy: schema.NamingStrategy{TablePrefix: strategy.prefix},
			})
			require.NoError(t, err)

			for _, modelCase := range models {
				t.Run(modelCase.name, func(t *testing.T) {
					statement := &gorm.Statement{DB: db}
					require.NoError(t, statement.Parse(modelCase.value))
					assert.Equal(t, strategy.prefix+modelCase.tableName, statement.Schema.Table)
				})
			}
		})
	}
}
