package controller

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func executeAppTaskSubmission(c *gin.Context, info *relaycommon.RelayInfo, submit taskSubmitAttempt) (*taskSubmissionOutcome, *dto.TaskError) {
	client := c.Request
	ctx, cancel := context.WithTimeout(context.WithoutCancel(client.Context()), 30*time.Second)
	defer cancel()
	c.Request = client.Clone(ctx)
	defer func() { c.Request = client }()
	result, taskErr := submit(c, info)
	if taskErr != nil {
		return nil, taskErr
	}
	if result == nil || result.AppExecution == nil {
		return nil, service.TaskErrorWrapperLocal(errors.New("invalid_app_execution"), "invalid_app_execution", http.StatusInternalServerError)
	}
	task, err := relay.AppTaskProjection(c, result.AppExecution)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(errors.New("submission_unknown"), "submission_unknown", http.StatusServiceUnavailable)
	}
	return &taskSubmissionOutcome{Result: result, Task: task, RelayInfo: info}, nil
}
