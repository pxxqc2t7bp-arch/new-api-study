package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeferredTaskClaimRecoversExpiredLeaseAndCompletes(t *testing.T) {
	truncateTables(t)
	task := &Task{
		TaskID:         "task_deferred_claim",
		Status:         TaskStatusNotStart,
		Progress:       "0%",
		ExecutionMode:  TaskExecutionModeDeferred,
		DispatchStatus: TaskDispatchStatusPending,
		PrivateData: TaskPrivateData{
			DeferredRequest: &TaskDeferredRequest{
				Path:        "/v1/responses",
				Method:      "POST",
				RequestBody: json.RawMessage(`{"model":"image-model","prompt":"hello"}`),
			},
		},
	}
	insertTask(t, task)

	assert.True(t, HasDispatchableDeferredTasks(100))
	candidates, err := FindDispatchableDeferredTasks(100, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	claimed, won, err := ClaimDeferredTask(task.ID, "runner-a", 100, 200)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, TaskDispatchStatusRunning, claimed.DispatchStatus)
	assert.Equal(t, 1, claimed.DispatchAttempts)
	assert.False(t, HasDispatchableDeferredTasks(150))

	_, won, err = ClaimDeferredTask(task.ID, "runner-b", 150, 250)
	require.NoError(t, err)
	assert.False(t, won)

	claimed, won, err = ClaimDeferredTask(task.ID, "runner-b", 201, 301)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, 2, claimed.DispatchAttempts)

	won, err = RequeueDeferredTask(claimed, "runner-b", "temporary failure")
	require.NoError(t, err)
	require.True(t, won)
	assert.True(t, HasDispatchableDeferredTasks(202))

	claimed, won, err = ClaimDeferredTask(task.ID, "runner-c", 202, 302)
	require.NoError(t, err)
	require.True(t, won)
	claimed.Status = TaskStatusSubmitted
	claimed.Progress = "10%"
	claimed.PrivateData.UpstreamTaskID = "upstream-private"
	claimed.PrivateData.DeferredRequest = nil
	won, err = CompleteDeferredTask(claimed, "runner-c")
	require.NoError(t, err)
	require.True(t, won)

	var stored Task
	require.NoError(t, DB.First(&stored, task.ID).Error)
	assert.Equal(t, TaskDispatchStatusDispatched, stored.DispatchStatus)
	assert.Equal(t, TaskStatus(TaskStatusSubmitted), stored.Status)
	assert.Equal(t, "upstream-private", stored.PrivateData.UpstreamTaskID)
	assert.Nil(t, stored.PrivateData.DeferredRequest)
	assert.False(t, HasDispatchableDeferredTasks(400))
}

func TestTaskPollingExcludesUndispatchedDeferredTasks(t *testing.T) {
	truncateTables(t)
	tasks := []*Task{
		{
			TaskID:         "task_deferred_pending",
			Status:         TaskStatusNotStart,
			Progress:       "0%",
			ExecutionMode:  TaskExecutionModeDeferred,
			DispatchStatus: TaskDispatchStatusPending,
		},
		{
			TaskID:         "task_deferred_dispatched",
			Status:         TaskStatusSubmitted,
			Progress:       "10%",
			ExecutionMode:  TaskExecutionModeDeferred,
			DispatchStatus: TaskDispatchStatusDispatched,
			PrivateData:    TaskPrivateData{UpstreamTaskID: "upstream-deferred"},
		},
		{
			TaskID:      "task_legacy",
			Status:      TaskStatusSubmitted,
			Progress:    "10%",
			PrivateData: TaskPrivateData{UpstreamTaskID: "upstream-legacy"},
		},
	}
	for _, task := range tasks {
		insertTask(t, task)
	}

	polled := GetAllUnFinishSyncTasks(10)
	ids := make([]string, 0, len(polled))
	for _, task := range polled {
		ids = append(ids, task.TaskID)
	}
	assert.ElementsMatch(t, []string{"task_deferred_dispatched", "task_legacy"}, ids)
}
