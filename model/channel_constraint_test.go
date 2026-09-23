package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterCandidateIDs(t *testing.T) {
	alphaSetting := `{"task_plugin_key":"alpha"}`
	betaSetting := `{"task_plugin_key":"beta"}`
	alpha := &Channel{Id: 900001, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Setting: &alphaSetting}
	beta := &Channel{Id: 900002, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Setting: &betaSetting}
	ordinary := &Channel{Id: 900003, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled}
	kling := &Channel{Id: 900004, Type: constant.ChannelTypeKling, Status: common.ChannelStatusEnabled}
	jimeng := &Channel{Id: 900005, Type: constant.ChannelTypeJimeng, Status: common.ChannelStatusEnabled}
	matchingCustom := &Channel{Id: 900010, Type: constant.ChannelTypeAdvancedCustom, Status: common.ChannelStatusEnabled}
	matchingCustom.SetOtherSettings(kitdto.ChannelOtherSettings{
		AdvancedCustom: &kitdto.AdvancedCustomConfig{
			Routes: []kitdto.AdvancedCustomRoute{{
				IncomingPath: "/v1/chat/completions",
				Models:       []string{"gpt-4"},
			}},
		},
	})
	otherCustom := &Channel{Id: 900011, Type: constant.ChannelTypeAdvancedCustom, Status: common.ChannelStatusEnabled}
	otherCustom.SetOtherSettings(kitdto.ChannelOtherSettings{
		AdvancedCustom: &kitdto.AdvancedCustomConfig{
			Routes: []kitdto.AdvancedCustomRoute{{
				IncomingPath: "/v1/responses",
				Models:       []string{"gpt-4"},
			}},
		},
	})
	cxy := &Channel{Id: 900012, Type: constant.ChannelTypeVolcEngine, Status: common.ChannelStatusEnabled}
	cxy.SetOtherSettings(kitdto.ChannelOtherSettings{RoutingAccount: kitdto.RoutingAccountCXY})
	support := &Channel{Id: 900013, Type: constant.ChannelTypeVolcEngine, Status: common.ChannelStatusEnabled}
	support.SetOtherSettings(kitdto.ChannelOtherSettings{RoutingAccount: kitdto.RoutingAccountSupport})

	pathFilter := dto.ChannelFilter{Kind: dto.FilterRequestPath, RequestPath: "/v1/chat/completions"}
	emptyPathFilter := dto.ChannelFilter{Kind: dto.FilterRequestPath, RequestPath: ""}

	tests := []struct {
		name      string
		ids       []int
		modelName string
		filters   []dto.ChannelFilter
		wantKept  []int
		wantEmpty dto.ChannelFilterKind
	}{
		{
			name:      "identity keeps matching type-59 key",
			ids:       []int{900001, 900002},
			modelName: "shared",
			filters:   identityFilters("alpha", nil),
			wantKept:  []int{900001},
		},
		{
			name:      "identity empty key drops all type-59",
			ids:       []int{900001, 900002},
			modelName: "shared",
			filters:   identityFilters("", nil),
			wantKept:  []int{},
			wantEmpty: dto.FilterTaskPluginIdentity,
		},
		{
			name:      "identity empty key keeps ordinary channel",
			ids:       []int{900003},
			modelName: "ordinary",
			filters:   identityFilters("", nil),
			wantKept:  []int{900003},
		},
		{
			name:      "identity keeps matching legacy type",
			ids:       []int{900004, 900005},
			modelName: "legacy",
			filters:   identityFilters("legacy-alpha", []int{constant.ChannelTypeKling}),
			wantKept:  []int{900004},
		},
		{
			name:      "identity keeps all listed legacy types",
			ids:       []int{900004, 900005},
			modelName: "legacy",
			filters:   identityFilters("legacy-alpha", []int{constant.ChannelTypeKling, constant.ChannelTypeJimeng}),
			wantKept:  []int{900004, 900005},
		},
		{
			name:      "identity keyed with no types drops legacy",
			ids:       []int{900004, 900005},
			modelName: "legacy",
			filters:   identityFilters("legacy-alpha", nil),
			wantKept:  []int{},
			wantEmpty: dto.FilterTaskPluginIdentity,
		},
		{
			name:      "identity drops missing cache entry",
			ids:       []int{900004, 999999},
			modelName: "legacy",
			filters:   identityFilters("legacy-alpha", []int{constant.ChannelTypeKling}),
			wantKept:  []int{900004},
		},
		{
			name:      "empty request path is a passthrough including missing ids",
			ids:       []int{900003, 900010, 999999},
			modelName: "gpt-4",
			filters:   []dto.ChannelFilter{emptyPathFilter},
			wantKept:  []int{900003, 900010, 999999},
		},
		{
			name:      "request path keeps missing cache entry for consistency",
			ids:       []int{900003, 999999},
			modelName: "gpt-4",
			filters:   []dto.ChannelFilter{pathFilter},
			wantKept:  []int{900003, 999999},
		},
		{
			name:      "request path keeps matching type-58 and ordinary",
			ids:       []int{900003, 900010, 900011},
			modelName: "gpt-4",
			filters:   []dto.ChannelFilter{pathFilter},
			wantKept:  []int{900003, 900010},
		},
		{
			name:      "request path empties when only unmatched type-58 remains",
			ids:       []int{900011},
			modelName: "gpt-4",
			filters:   []dto.ChannelFilter{pathFilter},
			wantKept:  []int{},
			wantEmpty: dto.FilterRequestPath,
		},
		{
			name:      "intersection attributes empty set to identity after path keeps candidates",
			ids:       []int{900001, 900010},
			modelName: "gpt-4",
			filters:   []dto.ChannelFilter{pathFilter, identityFilters("missing", nil)[0]},
			wantKept:  []int{},
			wantEmpty: dto.FilterTaskPluginIdentity,
		},
		{
			name:      "intersection attributes empty set to path when path runs first",
			ids:       []int{900011},
			modelName: "gpt-4",
			filters:   []dto.ChannelFilter{identityFilters("", nil)[0], pathFilter},
			wantKept:  []int{},
			wantEmpty: dto.FilterRequestPath,
		},
		{
			name:      "routing account keeps only matching channel",
			ids:       []int{900012, 900013, 900003},
			modelName: "media",
			filters: []dto.ChannelFilter{{
				Kind:           dto.FilterRoutingAccount,
				RoutingAccount: kitdto.RoutingAccountCXY,
			}},
			wantKept: []int{900012},
		},
		{
			name:      "routing account fails closed for missing cache entry",
			ids:       []int{999999},
			modelName: "media",
			filters: []dto.ChannelFilter{{
				Kind:           dto.FilterRoutingAccount,
				RoutingAccount: kitdto.RoutingAccountCXY,
			}},
			wantKept:  []int{},
			wantEmpty: dto.FilterRoutingAccount,
		},
	}

	channelSyncLock.Lock()
	previous := channelsIDM
	channelsIDM = map[int]*Channel{
		900001: alpha,
		900002: beta,
		900003: ordinary,
		900004: kling,
		900005: jimeng,
		900010: matchingCustom,
		900011: otherCustom,
		900012: cxy,
		900013: support,
	}
	t.Cleanup(func() {
		channelsIDM = previous
		channelSyncLock.Unlock()
	})

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			kept, emptiedBy := filterCandidateIDs(testCase.ids, testCase.modelName, testCase.filters)
			if testCase.wantKept == nil {
				assert.Nil(t, kept)
			} else {
				assert.Equal(t, testCase.wantKept, kept)
			}
			assert.Equal(t, testCase.wantEmpty, emptiedBy)
		})
	}
}

func TestChannelSatisfiesFilters(t *testing.T) {
	alphaSetting := `{"task_plugin_key":"alpha"}`
	alpha := &Channel{Id: 1, Type: constant.ChannelTypeTaskPlugin, Setting: &alphaSetting}
	ordinary := &Channel{Id: 2, Type: constant.ChannelTypeOpenAI}
	custom := &Channel{Id: 3, Type: constant.ChannelTypeAdvancedCustom}
	custom.SetOtherSettings(kitdto.ChannelOtherSettings{
		AdvancedCustom: &kitdto.AdvancedCustomConfig{
			Routes: []kitdto.AdvancedCustomRoute{{
				IncomingPath: "/v1/chat/completions",
				Models:       []string{"gpt-4"},
			}},
		},
	})

	ok, kind := ChannelSatisfiesFilters(nil, "gpt-4", nil)
	assert.False(t, ok)
	assert.Equal(t, dto.ChannelFilterKind(""), kind)

	ok, kind = ChannelSatisfiesFilters(alpha, "shared", identityFilters("alpha", nil))
	require.True(t, ok)
	assert.Equal(t, dto.ChannelFilterKind(""), kind)

	ok, kind = ChannelSatisfiesFilters(alpha, "shared", identityFilters("beta", nil))
	assert.False(t, ok)
	assert.Equal(t, dto.FilterTaskPluginIdentity, kind)

	ok, kind = ChannelSatisfiesFilters(ordinary, "gpt-4", []dto.ChannelFilter{{
		Kind:        dto.FilterRequestPath,
		RequestPath: "/v1/chat/completions",
	}})
	require.True(t, ok)
	assert.Equal(t, dto.ChannelFilterKind(""), kind)

	ok, kind = ChannelSatisfiesFilters(custom, "gpt-4", []dto.ChannelFilter{{
		Kind:        dto.FilterRequestPath,
		RequestPath: "/v1/responses",
	}})
	assert.False(t, ok)
	assert.Equal(t, dto.FilterRequestPath, kind)

	cxy := &Channel{Id: 4, Type: constant.ChannelTypeVolcEngine}
	cxy.SetOtherSettings(kitdto.ChannelOtherSettings{RoutingAccount: kitdto.RoutingAccountCXY})
	support := &Channel{Id: 5, Type: constant.ChannelTypeVolcEngine}
	support.SetOtherSettings(kitdto.ChannelOtherSettings{RoutingAccount: kitdto.RoutingAccountSupport})
	accountFilter := []dto.ChannelFilter{{
		Kind:           dto.FilterRoutingAccount,
		RoutingAccount: kitdto.RoutingAccountCXY,
	}}
	ok, kind = ChannelSatisfiesFilters(cxy, "media", accountFilter)
	require.True(t, ok)
	assert.Empty(t, kind)
	ok, kind = ChannelSatisfiesFilters(support, "media", accountFilter)
	assert.False(t, ok)
	assert.Equal(t, dto.FilterRoutingAccount, kind)
}

func TestChannelSatisfiesGeminiLiveTypeFilter(t *testing.T) {
	filter := []dto.ChannelFilter{{
		Kind:                dto.FilterChannelTypes,
		AllowedChannelTypes: []int{constant.ChannelTypeGemini},
	}}

	ok, kind := ChannelSatisfiesFilters(
		&Channel{Type: constant.ChannelTypeGemini},
		"gemini-live-test",
		filter,
	)
	require.True(t, ok)
	assert.Empty(t, kind)

	for _, channelType := range []int{
		constant.ChannelTypeVertexAi,
		constant.ChannelTypeOpenAI,
	} {
		ok, kind = ChannelSatisfiesFilters(
			&Channel{Type: channelType},
			"gemini-live-test",
			filter,
		)
		assert.False(t, ok)
		assert.Equal(t, dto.FilterChannelTypes, kind)
	}
}

func TestChannelSatisfiesGeminiLiveCapabilityFilter(t *testing.T) {
	filterKind := dto.FilterGeminiLive
	filter := []dto.ChannelFilter{{Kind: filterKind}}
	enabled := &Channel{
		Type:          constant.ChannelTypeGemini,
		OtherSettings: `{"gemini_live_enabled":true}`,
	}

	ok, kind := ChannelSatisfiesFilters(enabled, "gemini-live-test", filter)
	require.True(t, ok)
	assert.Empty(t, kind)

	for _, channel := range []*Channel{
		{Type: constant.ChannelTypeGemini},
		{
			Type:          constant.ChannelTypeGemini,
			OtherSettings: `{"gemini_live_enabled":false}`,
		},
	} {
		ok, kind = ChannelSatisfiesFilters(channel, "gemini-live-test", filter)
		assert.False(t, ok)
		assert.Equal(t, filterKind, kind)
	}
}
