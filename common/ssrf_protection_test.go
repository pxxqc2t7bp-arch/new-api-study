package common

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSSRFProtectionRejectsLiteralPrivateAndReservedIPs(t *testing.T) {
	protection := &SSRFProtection{
		AllowPrivateIp:   false,
		DomainFilterMode: false,
		IpFilterMode:     false,
	}

	tests := []string{
		"127.0.0.1",
		"10.0.0.1",
		"169.254.169.254",
		"fc00::1",
		"::ffff:127.0.0.1",
	}
	for _, host := range tests {
		t.Run(host, func(t *testing.T) {
			require.Error(t, protection.ValidateNetworkTarget(host, 80))
		})
	}
}

func TestSSRFProtectionAllowsPrivateIPWhenExplicitlyEnabled(t *testing.T) {
	protection := &SSRFProtection{
		AllowPrivateIp:   true,
		DomainFilterMode: false,
		IpFilterMode:     false,
	}

	require.NoError(t, protection.ValidateNetworkTarget("10.0.0.1", 80))
}

func TestSSRFProtectionRejectsResolvedPrivateIP(t *testing.T) {
	protection := &SSRFProtection{
		AllowPrivateIp:         false,
		DomainFilterMode:       false,
		IpFilterMode:           false,
		ApplyIPFilterForDomain: true,
	}

	require.NoError(t, protection.ValidateNetworkTarget("example.com", 80))
	require.Error(t, protection.ValidateResolvedIP("example.com", net.ParseIP("169.254.169.254")))
}

func TestNewSSRFProtectionFromFetchSettingParsesPortRanges(t *testing.T) {
	protection, err := NewSSRFProtectionFromFetchSetting(false, false, false, nil, nil, []string{"80", "8000-8001"}, true)
	require.NoError(t, err)

	require.NoError(t, protection.ValidateNetworkTarget("example.com", 8001))
	require.Error(t, protection.ValidateNetworkTarget("example.com", 9000))
}

func TestSSRFProtectionWildcardRequiresSubdomain(t *testing.T) {
	domainList := []string{"  *.TOS-CN-BEIJING.VOLCES.COM  "}
	validate := func(domain string) error {
		return ValidateURLWithFetchSetting(
			"https://"+domain+"/object",
			true, false, true, false,
			domainList, nil, []string{"443"}, false,
		)
	}
	tests := []struct {
		name    string
		domain  string
		allowed bool
	}{
		{name: "subdomain", domain: "ARK-PROJECT.TOS-CN-BEIJING.VOLCES.COM", allowed: true},
		{name: "apex", domain: "tos-cn-beijing.volces.com", allowed: false},
		{name: "lookalike prefix", domain: "evil-tos-cn-beijing.volces.com", allowed: false},
		{name: "lookalike suffix", domain: "tos-cn-beijing.volces.com.evil", allowed: false},
	}

	for _, context := range []string{"initial", "redirect"} {
		t.Run(context, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					require.Equal(t, test.allowed, isDomainListed(test.domain, domainList))
					if test.allowed {
						require.NoError(t, validate(test.domain))
					} else {
						require.Error(t, validate(test.domain))
					}
				})
			}
		})
	}
}
