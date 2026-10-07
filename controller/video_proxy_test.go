package controller

import (
	"net/http"
	"slices"
	"testing"

	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/require"
)

func TestTaskMediaWildcardDomainBoundary(t *testing.T) {
	fetchSetting := system_setting.GetFetchSetting()
	original := *fetchSetting
	original.DomainList = slices.Clone(fetchSetting.DomainList)
	original.IpList = slices.Clone(fetchSetting.IpList)
	original.AllowedPorts = slices.Clone(fetchSetting.AllowedPorts)
	t.Cleanup(func() {
		*fetchSetting = original
	})

	*fetchSetting = system_setting.FetchSetting{
		EnableSSRFProtection:   true,
		AllowPrivateIp:         false,
		DomainFilterMode:       true,
		IpFilterMode:           false,
		DomainList:             []string{"*.tos-cn-beijing.volces.com"},
		IpList:                 nil,
		AllowedPorts:           []string{"443"},
		ApplyIPFilterForDomain: false,
	}

	redirectClient := taskMediaRedirectClient(&http.Client{}, "", nil, nil, true)
	paths := []struct {
		name     string
		validate func(*testing.T, string) error
	}{
		{
			name: "initial",
			validate: func(_ *testing.T, rawURL string) error {
				return validateTaskMediaURL(rawURL, "")
			},
		},
		{
			name: "redirect",
			validate: func(t *testing.T, rawURL string) error {
				request, err := http.NewRequest(http.MethodGet, rawURL, nil)
				require.NoError(t, err)
				return redirectClient.CheckRedirect(request, nil)
			},
		},
	}
	tests := []struct {
		name    string
		host    string
		allowed bool
	}{
		{name: "valid subdomain", host: "ark-project.tos-cn-beijing.volces.com", allowed: true},
		{name: "apex", host: "tos-cn-beijing.volces.com", allowed: false},
		{name: "empty leading label", host: ".tos-cn-beijing.volces.com", allowed: false},
		{name: "two empty leading labels", host: "..tos-cn-beijing.volces.com", allowed: false},
		{name: "empty interior label", host: "a..tos-cn-beijing.volces.com", allowed: false},
		{name: "prefix lookalike", host: "evil-tos-cn-beijing.volces.com", allowed: false},
		{name: "suffix lookalike", host: "tos-cn-beijing.volces.com.evil", allowed: false},
	}

	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					err := path.validate(t, "https://"+test.host+"/object")
					if test.allowed {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
						if path.name == "redirect" {
							require.ErrorIs(t, err, errTaskMediaRequestRejected)
						}
					}
				})
			}
		})
	}
}
