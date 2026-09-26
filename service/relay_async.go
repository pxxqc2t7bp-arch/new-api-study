package service

import (
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

const relayAsyncRunnerContextKey = "service_relay_async_runner"

// RelayAsyncRunner schedules background work associated with one relay request.
type RelayAsyncRunner func(task func())

// SetRelayAsyncRunner overrides relay background scheduling for the current request.
func SetRelayAsyncRunner(c *gin.Context, runner RelayAsyncRunner) {
	if c == nil {
		return
	}
	c.Set(relayAsyncRunnerContextKey, runner)
}

// RunRelayAsync schedules task with the request runner or the shared worker pool.
func RunRelayAsync(c *gin.Context, task func()) {
	if c != nil {
		if value, exists := c.Get(relayAsyncRunnerContextKey); exists {
			if runner, ok := value.(RelayAsyncRunner); ok && runner != nil {
				runner(task)
				return
			}
		}
	}
	gopool.Go(task)
}
