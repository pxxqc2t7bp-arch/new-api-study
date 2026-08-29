package common

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserSessionIdleTimeoutEnvironment(t *testing.T) {
	previous := UserSessionIdleTimeoutSeconds
	t.Cleanup(func() { UserSessionIdleTimeoutSeconds = previous })

	t.Setenv("USER_SESSION_IDLE_TIMEOUT_SECONDS", "1800")
	initUserSessionSettings()
	assert.Equal(t, int64(1800), UserSessionIdleTimeoutSeconds)

	t.Setenv("USER_SESSION_IDLE_TIMEOUT_SECONDS", strconv.Itoa(-1))
	initUserSessionSettings()
	assert.Equal(t, int64(DefaultUserSessionIdleTimeoutSeconds), UserSessionIdleTimeoutSeconds)
}
