package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func (f *appExecutionFixture) acceptedTask(t *testing.T) model.AppTaskExecution {
	t.Helper()
	grant, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
	require.NoError(t, err)
	task := model.AppTaskExecution{
		TaskID: "task-accepted", GrantID: grant.GrantID, AppKey: f.request.AppKey,
		InstallationID: f.installation.InstallationID, AppSessionID: f.request.AppSessionID,
		Subject: f.request.Subject, UserID: f.user.Id, RunID: f.request.RunID,
		ExecutionRequestID: f.request.ExecutionRequestID, Operation: f.request.Operation,
		SubmissionHash: serviceDigest("accepted-submission"), ProviderAccepted: true, Status: "accepted",
		ActualModel: appExecutionTestModel, PluginKey: "doubao", PluginVersion: "1.2.0",
		LogicalHash:   serviceDigest("accepted-logical-operation"),
		ProviderState: "running", UpdatedAt: f.options.Now().Unix(),
		UsageJSON: `{"output_tokens":17}`, ArtifactsJSON: `[{"type":"video","url":"https://media.example.com/result.mp4"}]`,
		ResultJSON: `{"error":null}`,
	}
	require.NoError(t, f.db.Create(&task).Error)
	return task
}

func TestTaskLookupAllowsAcceptedReconciliationAfterLogout(t *testing.T) {
	f := newAppExecutionFixture(t)
	task := f.acceptedTask(t)
	request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: f.request.AppKey,
		AppSessionID: f.request.AppSessionID, Subject: f.request.Subject, TaskID: &task.TaskID, GrantID: nil}
	active, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.False(t, active.ReconciliationOnly)
	f.logout(t, false)
	loggedOut, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.True(t, loggedOut.ReconciliationOnly)
	assert.True(t, loggedOut.ProviderAccepted)
	assert.Equal(t, task.TaskID, loggedOut.TaskID)
	assert.Equal(t, task.GrantID, loggedOut.GrantID)
	assert.Equal(t, task.ActualModel, loggedOut.ActualModel)
	loggedOut.ReconciliationOnly = false
	assert.Equal(t, active, loggedOut, "logout preserves all persisted facts")
	for _, reason := range []string{"password_changed", "admin_revoke", ""} {
		require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.session.SID).
			Update("revoked_reason", reason).Error)
		_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
		require.Error(t, err, "only an explicit dashboard logout permits reconciliation reads")
	}
	var after model.AppTaskExecution
	require.NoError(t, f.db.Where("task_id = ?", task.TaskID).First(&after).Error)
	assert.Equal(t, task, after)
	var payer model.User
	require.NoError(t, f.db.First(&payer, f.user.Id).Error)
	assert.Equal(t, 1000000, payer.Quota)
	for _, table := range []any{&model.AppTaskSettlement{}, &model.AppTaskOutbox{}} {
		var count int64
		require.NoError(t, f.db.Model(table).Count(&count).Error)
		assert.Zero(t, count)
	}
}

func TestTaskLookupMapsCredentialQueryFailureToServiceUnavailable(t *testing.T) {
	f := newAppExecutionFixture(t)
	task := f.acceptedTask(t)
	request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: f.request.AppKey,
		AppSessionID: f.request.AppSessionID, Subject: f.request.Subject, TaskID: &task.TaskID}
	injected := errors.New("injected credential query failure")
	var failed atomic.Bool
	const callbackName = "test:task-authority-credential-query-failure"
	require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "app_service_credentials" && failed.CompareAndSwap(false, true) {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, f.db.Callback().Query().Remove(callbackName))
	})

	_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)

	var authErr *AppPluginAuthError
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, "service_unavailable", authErr.Code)
	assert.NotContains(t, err.Error(), injected.Error())
}

func TestAppTaskControlMapsStorageQueryFailuresToServiceUnavailable(t *testing.T) {
	queries := []struct {
		name           string
		table          string
		lookup, cancel bool
		prepare        func(*testing.T, *appExecutionFixture, model.AppTaskExecution)
	}{
		{name: "installation", table: "app_installations", lookup: true, cancel: true},
		{name: "app session", table: "app_plugin_sessions", lookup: true, cancel: true},
		{name: "dashboard user", table: "users", lookup: true, cancel: true},
		{name: "dashboard session", table: "user_sessions", lookup: true, cancel: true},
		{name: "registration", table: "app_versions", lookup: true, cancel: true},
		{name: "task", table: "app_task_executions", lookup: true, cancel: true},
		{name: "grant", table: "app_execution_grants", lookup: true, cancel: true},
		{
			name: "artifact projection", table: "tasks", lookup: true,
			prepare: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) {
				t.Helper()
				require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).
					Updates(map[string]any{
						"provider_state": "succeeded",
						"artifacts_json": `[{"key":"result"}]`,
					}).Error)
			},
		},
		{name: "cancel replay", table: "app_task_cancel_replays", cancel: true},
	}
	operations := []struct {
		name string
		call func(*testing.T, *appExecutionFixture, model.AppTaskExecution) error
	}{
		{
			name: "lookup",
			call: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) error {
				t.Helper()
				request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
					AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: &task.TaskID}
				_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
				return err
			},
		},
		{
			name: "cancel",
			call: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) error {
				t.Helper()
				request := AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
					AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: task.TaskID,
					Reason: "user_requested"}
				_, err := f.execution.CancelTask(t.Context(), f.serviceID, uuid.NewString(), request)
				return err
			},
		},
	}
	for _, operation := range operations {
		for _, query := range queries {
			if operation.name == "lookup" && !query.lookup || operation.name == "cancel" && !query.cancel {
				continue
			}
			t.Run(operation.name+"/"+query.name, func(t *testing.T) {
				f := newAppExecutionFixture(t)
				task := f.acceptedTask(t)
				if query.prepare != nil {
					query.prepare(t, f, task)
				}
				privateDetail := "private " + query.name + " storage detail"
				injected := errors.New(privateDetail)
				var failed atomic.Bool
				callbackName := "test:app-task-query-failure:" + uuid.NewString()
				require.NoError(t, f.db.Callback().Query().Before("gorm:query").
					Register(callbackName, func(tx *gorm.DB) {
						if tx.Statement.Table == query.table && failed.CompareAndSwap(false, true) {
							tx.AddError(injected)
						}
					}))
				t.Cleanup(func() {
					require.NoError(t, f.db.Callback().Query().Remove(callbackName))
				})

				err := operation.call(t, f, task)

				require.True(t, failed.Load(), "the intended storage query must be exercised")
				var authErr *AppPluginAuthError
				require.ErrorAs(t, err, &authErr)
				assert.Equal(t, "service_unavailable", authErr.Code)
				assert.Equal(t, http.StatusServiceUnavailable, AppRelayErrorStatus(err))
				assert.NotContains(t, err.Error(), privateDetail)
			})
		}
	}
}

func TestAppTaskControlMapsTransactionBoundaryFailuresToServiceUnavailable(t *testing.T) {
	operations := []struct {
		name string
		call func(*testing.T, *AppExecutionService, *appExecutionFixture, model.AppTaskExecution) error
	}{
		{
			name: "lookup",
			call: func(
				t *testing.T,
				execution *AppExecutionService,
				f *appExecutionFixture,
				task model.AppTaskExecution,
			) error {
				t.Helper()
				request := AppTaskLookupRequest{
					RequestID: uuid.NewString(), AppKey: task.AppKey,
					AppSessionID: task.AppSessionID, Subject: task.Subject,
					TaskID: &task.TaskID,
				}
				_, err := execution.LookupScopedTask(t.Context(), f.serviceID, request)
				return err
			},
		},
		{
			name: "cancel",
			call: func(
				t *testing.T,
				execution *AppExecutionService,
				f *appExecutionFixture,
				task model.AppTaskExecution,
			) error {
				t.Helper()
				request := AppTaskCancelRequest{
					RequestID: uuid.NewString(), AppKey: task.AppKey,
					AppSessionID: task.AppSessionID, Subject: task.Subject,
					TaskID: task.TaskID, Reason: "user_requested",
				}
				_, err := execution.CancelTask(
					t.Context(), f.serviceID, uuid.NewString(), request,
				)
				return err
			},
		},
	}
	boundaries := []struct {
		name      string
		beginErr  error
		commitErr error
	}{
		{name: "begin", beginErr: errors.New("private task control begin detail")},
		{name: "commit", commitErr: errors.New("private task control commit detail")},
	}
	for _, operation := range operations {
		for _, boundary := range boundaries {
			t.Run(operation.name+"/"+boundary.name, func(t *testing.T) {
				f := newAppExecutionFixture(t)
				task := f.acceptedTask(t)
				faultDB := appExecutionDBWithTransactionFault(
					t, f.db, boundary.beginErr, boundary.commitErr,
				)
				execution := NewAppExecutionService(faultDB, f.options)

				err := operation.call(t, execution, f, task)

				privateDetail := boundary.beginErr
				if privateDetail == nil {
					privateDetail = boundary.commitErr
				}
				var authErr *AppPluginAuthError
				require.ErrorAs(t, err, &authErr)
				assert.Equal(t, "service_unavailable", authErr.Code)
				assert.Equal(t, http.StatusServiceUnavailable, AppRelayErrorStatus(err))
				assert.NotContains(t, err.Error(), privateDetail.Error())
				var replays int64
				require.NoError(t, f.db.Model(&model.AppTaskCancelReplay{}).Count(&replays).Error)
				assert.Zero(t, replays)
			})
		}
	}
}

func TestAppTaskControlKeepsBusinessDenialsNonRetryable(t *testing.T) {
	tests := []struct {
		name       string
		invalidate func(*testing.T, *appExecutionFixture, model.AppTaskExecution)
		wantCode   string
		wantStatus int
	}{
		{
			name: "installation not found",
			invalidate: func(_ *testing.T, f *appExecutionFixture, _ model.AppTaskExecution) {
				f.serviceID.InstallationID = "missing-installation"
			},
			wantCode:   "not_found",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "installation inactive",
			invalidate: func(t *testing.T, f *appExecutionFixture, _ model.AppTaskExecution) {
				require.NoError(t, f.db.Model(&model.AppInstallation{}).
					Where("installation_id = ?", f.installation.InstallationID).
					Update("status", model.AppInstallationStatusDisabled).Error)
			},
			wantCode:   "identity_inactive",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "session expired",
			invalidate: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) {
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).
					Where("app_session_id = ?", task.AppSessionID).
					Update("upstream_expires_at", f.options.Now().Unix()).Error)
			},
			wantCode:   "unauthenticated",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "session revoked",
			invalidate: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) {
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).
					Where("app_session_id = ?", task.AppSessionID).
					Update("revoked_at", f.options.Now().Unix()).Error)
			},
			wantCode:   "unauthenticated",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "session scope denied",
			invalidate: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) {
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).
					Where("app_session_id = ?", task.AppSessionID).
					Update("granted_scopes", `["identity.read"]`).Error)
			},
			wantCode:   "scope_denied",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "registration denied",
			invalidate: func(t *testing.T, f *appExecutionFixture, _ model.AppTaskExecution) {
				require.NoError(t, f.db.Model(&model.AppVersion{}).
					Where("id = ?", f.installation.AppVersionID).
					Update("manifest_sha256", strings.Repeat("0", 64)).Error)
			},
			wantCode:   "scope_denied",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "grant not found",
			invalidate: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) {
				require.NoError(t, f.db.Where("grant_id = ?", task.GrantID).
					Delete(&model.AppExecutionGrant{}).Error)
			},
			wantCode:   "not_found",
			wantStatus: http.StatusNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			task := f.acceptedTask(t)
			test.invalidate(t, f, task)
			request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
				AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: &task.TaskID}

			_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)

			var authErr *AppPluginAuthError
			require.ErrorAs(t, err, &authErr)
			assert.Equal(t, test.wantCode, authErr.Code)
			status := AppRelayErrorStatus(err)
			assert.Equal(t, test.wantStatus, status)
			assert.Less(t, status, http.StatusInternalServerError)
		})
	}
}

func TestAppTaskArtifactContentPreservesStorageFailures(t *testing.T) {
	for _, query := range []struct {
		name    string
		table   string
		prepare func(*testing.T, *appExecutionFixture, model.AppTaskExecution)
	}{
		{name: "installation", table: "app_installations"},
		{name: "task", table: "app_task_executions"},
		{
			name: "projection", table: "tasks",
			prepare: func(t *testing.T, f *appExecutionFixture, task model.AppTaskExecution) {
				t.Helper()
				require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).
					Updates(map[string]any{
						"provider_state": "succeeded",
						"artifacts_json": `[{"key":"result"}]`,
					}).Error)
			},
		},
	} {
		t.Run(query.name, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			task := f.acceptedTask(t)
			if query.prepare != nil {
				query.prepare(t, f, task)
			}
			access := AppTaskArtifactAccess{
				TaskID: task.TaskID, ArtifactKey: "result", AppKey: task.AppKey,
				InstallationID: task.InstallationID, GrantID: task.GrantID,
				AppSessionID: task.AppSessionID, CredentialID: f.serviceID.CredentialID,
				CredentialVersion: f.serviceID.Version, Subject: task.Subject,
				UserID: task.UserID, ExpiresAt: f.options.Now().Add(time.Minute).Unix(),
			}
			privateDetail := "private artifact " + query.name + " storage detail"
			var failed atomic.Bool
			callbackName := "test:app-task-artifact-query-failure:" + uuid.NewString()
			require.NoError(t, f.db.Callback().Query().Before("gorm:query").
				Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Table == query.table && failed.CompareAndSwap(false, true) {
						tx.AddError(errors.New(privateDetail))
					}
				}))
			t.Cleanup(func() {
				require.NoError(t, f.db.Callback().Query().Remove(callbackName))
			})

			_, _, err := f.execution.ResolveAppTaskArtifactContent(t.Context(), access, http.MethodGet)

			require.True(t, failed.Load(), "the intended storage query must be exercised")
			var authErr *AppPluginAuthError
			require.ErrorAs(t, err, &authErr)
			assert.Equal(t, "service_unavailable", authErr.Code)
			assert.Equal(t, http.StatusServiceUnavailable, AppRelayErrorStatus(err))
			assert.NotContains(t, err.Error(), privateDetail)
		})
	}
}

func TestAppTaskArtifactContentMapsTransactionBoundaryFailuresToServiceUnavailable(t *testing.T) {
	for _, boundary := range []struct {
		name      string
		beginErr  error
		commitErr error
	}{
		{name: "begin", beginErr: errors.New("private artifact begin detail")},
		{name: "commit", commitErr: errors.New("private artifact commit detail")},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			task := f.acceptedTask(t)
			require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).
				Updates(map[string]any{
					"provider_state": "succeeded",
					"artifacts_json": `[{"key":"result"}]`,
				}).Error)
			require.NoError(t, f.db.Create(&model.Task{
				TaskID: task.TaskID, UserId: task.UserID, Status: model.TaskStatusSuccess,
				ExecutionMode: model.TaskExecutionModeAppManaged,
				PrivateData: model.TaskPrivateData{
					UpstreamTaskID:  "cgt-artifact-boundary",
					AppArtifactURLs: map[string]string{"result": "https://media.example.com/result.mp4"},
				},
			}).Error)
			access := AppTaskArtifactAccess{
				TaskID: task.TaskID, ArtifactKey: "result", AppKey: task.AppKey,
				InstallationID: task.InstallationID, GrantID: task.GrantID,
				AppSessionID: task.AppSessionID, CredentialID: f.serviceID.CredentialID,
				CredentialVersion: f.serviceID.Version, Subject: task.Subject,
				UserID: task.UserID, ExpiresAt: f.options.Now().Add(time.Minute).Unix(),
			}
			_, source, err := f.execution.ResolveAppTaskArtifactContent(
				t.Context(), access, http.MethodGet,
			)
			require.NoError(t, err)
			require.Equal(t, "https://media.example.com/result.mp4", source)
			faultDB := appExecutionDBWithTransactionFault(
				t, f.db, boundary.beginErr, boundary.commitErr,
			)
			execution := NewAppExecutionService(faultDB, f.options)

			_, _, err = execution.ResolveAppTaskArtifactContent(
				t.Context(), access, http.MethodGet,
			)

			var authErr *AppPluginAuthError
			require.ErrorAs(t, err, &authErr)
			assert.Equal(t, "service_unavailable", authErr.Code)
			assert.Equal(t, http.StatusServiceUnavailable, AppRelayErrorStatus(err))
			if boundary.beginErr != nil {
				assert.NotContains(t, err.Error(), boundary.beginErr.Error())
			}
			if boundary.commitErr != nil {
				assert.NotContains(t, err.Error(), boundary.commitErr.Error())
			}
		})
	}
}

// Only the external ownership-evidence source is replaced; delegation and
// candidate filtering remain production service and database operations.
type appArkSourceFixture struct {
	queries  []AppArkTaskQuery
	evidence []AppArkTaskEvidence
	err      error
}

func (s *appArkSourceFixture) Lookup(_ context.Context, query AppArkTaskQuery) ([]AppArkTaskEvidence, error) {
	s.queries = append(s.queries, query)
	if s.evidence != nil || s.err != nil {
		return s.evidence, s.err
	}
	return []AppArkTaskEvidence{
		{TaskID: query.TaskID, AccountRef: "host-account", ProjectID: "project-one",
			CreatedAt: query.StartAt.Add(time.Minute), RequestJSON: `{"model":"doubao-seedance-2-5-260628","prompt":"fixture"}`},
		{TaskID: query.TaskID, AccountRef: "another-account", ProjectID: "project-one",
			CreatedAt: query.StartAt.Add(time.Minute), RequestJSON: `{"prompt":"other-account-private"}`},
	}, nil
}

func setupAppArkImportLookup(t *testing.T) (*appExecutionFixture, AppArkImportLookupRequest) {
	t.Helper()
	f := newAppExecutionFixture(t)
	f.execution.ArkSource = &appArkSourceFixture{}
	start, end := f.options.Now().Add(-time.Hour), f.options.Now()
	delegations, err := common.Marshal(AppArkImportDelegations{Delegations: []AppArkImportDelegation{{
		InstallationID: f.installation.InstallationID,
		UserID:         f.user.Id,
		AccountRef:     "host-account",
		ProjectID:      "project-one",
		StartAt:        start,
		EndAt:          end,
	}}})
	require.NoError(t, err)
	_, err = PublishAppArkImportDelegations(
		t.Context(), f.db, f.publisher, delegations, f.options.Now(),
	)
	require.NoError(t, err)
	return f, AppArkImportLookupRequest{
		RequestID: uuid.NewString(), AppKey: f.request.AppKey,
		AppSessionID: f.request.AppSessionID, Subject: f.request.Subject,
		TaskID: "cgt-historical-task",
		QueryScope: AppArkQueryScope{
			ProjectID: "project-one", StartAt: start.Add(time.Minute), EndAt: end,
		},
	}
}

func TestArkImportLookupMapsPolicyAndTransactionFailuresToServiceUnavailable(t *testing.T) {
	for _, query := range []struct {
		name       string
		occurrence int32
	}{
		{name: "initial policy read", occurrence: 1},
		{name: "locked policy recheck", occurrence: 2},
	} {
		t.Run(query.name, func(t *testing.T) {
			f, request := setupAppArkImportLookup(t)
			privateDetail := "private Ark " + query.name + " detail"
			failed := registerAppExecutionDBFault(
				t, f.db, "query", "options", query.occurrence, errors.New(privateDetail),
			)

			_, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)

			require.True(t, failed.Load(), "the intended policy read must be exercised")
			var authErr *AppPluginAuthError
			require.ErrorAs(t, err, &authErr)
			assert.Equal(t, "service_unavailable", authErr.Code)
			assert.Equal(t, http.StatusServiceUnavailable, AppRelayErrorStatus(err))
			assert.NotContains(t, err.Error(), privateDetail)
		})
	}

	for _, boundary := range []struct {
		name             string
		beginErr         error
		commitErr        error
		commitOccurrence int32
	}{
		{name: "begin", beginErr: errors.New("private Ark begin detail"), commitOccurrence: 1},
		{name: "initial commit", commitErr: errors.New("private Ark initial commit detail"), commitOccurrence: 1},
		{name: "recheck commit", commitErr: errors.New("private Ark recheck commit detail"), commitOccurrence: 2},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			f, request := setupAppArkImportLookup(t)
			faultDB := appExecutionDBWithTransactionFaultAt(
				t, f.db, boundary.beginErr, boundary.commitErr, boundary.commitOccurrence,
			)
			execution := NewAppExecutionService(faultDB, f.options)
			execution.ArkSource = f.execution.ArkSource

			_, err := execution.LookupArkImport(t.Context(), f.serviceID, request)

			var authErr *AppPluginAuthError
			require.ErrorAs(t, err, &authErr)
			assert.Equal(t, "service_unavailable", authErr.Code)
			assert.Equal(t, http.StatusServiceUnavailable, AppRelayErrorStatus(err))
			if boundary.beginErr != nil {
				assert.NotContains(t, err.Error(), boundary.beginErr.Error())
			}
			if boundary.commitErr != nil {
				assert.NotContains(t, err.Error(), boundary.commitErr.Error())
			}
		})
	}
}

func TestArkImportLookupKeepsMissingPolicyAndScopeDenialNonRetryable(t *testing.T) {
	t.Run("missing policy", func(t *testing.T) {
		f, request := setupAppArkImportLookup(t)
		require.NoError(t, f.db.Where(
			"policy_key = ?", model.AppArkImportDelegationsKey,
		).Delete(&model.AppExecutionPolicyVersion{}).Error)

		_, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)

		var authErr *AppPluginAuthError
		require.ErrorAs(t, err, &authErr)
		assert.Equal(t, "not_found", authErr.Code)
		assert.Equal(t, http.StatusNotFound, AppRelayErrorStatus(err))
	})

	t.Run("scope denied", func(t *testing.T) {
		f, request := setupAppArkImportLookup(t)
		request.QueryScope.ProjectID = "other-project"

		_, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)

		var authErr *AppPluginAuthError
		require.ErrorAs(t, err, &authErr)
		assert.Equal(t, "scope_denied", authErr.Code)
		assert.Equal(t, http.StatusForbidden, AppRelayErrorStatus(err))
	})
}

func (f *appExecutionFixture) logout(t *testing.T, refresh bool) {
	t.Helper()
	if refresh {
		var stored model.UserSession
		require.NoError(t, f.db.Where("sid = ?", f.session.SID).First(&stored).Error)
		t.Logf("refresh fixture stored_length=%d supplied_length=%d padded=%t",
			len(stored.RefreshHash), len(f.session.RefreshHash),
			strings.TrimSpace(stored.RefreshHash) == f.session.RefreshHash && stored.RefreshHash != f.session.RefreshHash)
	}
	// The production logout clock is wall time; keep its fixture session live.
	// Use a real digest shape: PostgreSQL pads the older short CHAR(64) fixture.
	refreshHash := serviceDigest("task-control-refresh-fixture")
	require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.session.SID).
		Updates(map[string]any{"expires_at": max(f.session.ExpiresAt, time.Now().Add(time.Hour).Unix()),
			"refresh_hash": refreshHash}).Error)
	var revoked bool
	var err error
	if refresh {
		revoked, err = model.RevokeUserSessionByRefreshHash(f.session.SID, refreshHash, "logout")
	} else {
		revoked, err = model.RevokeUserSession(f.user.Id, f.session.SID, "logout")
	}
	require.NoError(t, err)
	require.True(t, revoked)
	var current model.UserSession
	require.NoError(t, f.db.Where("sid = ?", f.session.SID).First(&current).Error)
	assert.Equal(t, f.session.Version, current.Version)
	assert.Equal(t, f.session.UserAuthVersion, current.UserAuthVersion)
	assert.Equal(t, "logout", current.RevokedReason)
	assert.Equal(t, model.UserSessionStatusRevoked, current.Status)
}

type appControlNoHTTP struct{ calls atomic.Int64 }

func (transport *appControlNoHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("unexpected provider I/O")
}

func TestAppTaskControlCurrentAuthority(t *testing.T) {
	t.Run("logout permits only accepted original session", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		task := f.acceptedTask(t)
		var other model.AppPluginSession
		require.NoError(t, f.db.Where("app_session_id = ?", task.AppSessionID).First(&other).Error)
		other.AppSessionID = uuid.NewString()
		require.NoError(t, f.db.Create(&other).Error)
		f.logout(t, false)
		request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
			AppSessionID: other.AppSessionID, Subject: task.Subject, TaskID: &task.TaskID}
		_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
		require.ErrorContains(t, err, "not_found")
		request.AppSessionID = task.AppSessionID
		require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).UpdateColumn("provider_accepted", false).Error)
		_, err = f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
		require.ErrorContains(t, err, "not_found")
	})
	t.Run("new session same owner and another real user", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		task := f.acceptedTask(t)
		f.logout(t, true)
		newDashboard := f.session
		newDashboard.SID = uuid.NewString()
		require.NoError(t, f.db.Create(&newDashboard).Error)
		f.identity.SessionID = newDashboard.SID
		f.appLaunchFixture.request.TransactionID = uuid.NewString()
		current := f.exchange(t)
		require.NotEqual(t, task.AppSessionID, current.AppSessionID)
		request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
			AppSessionID: current.AppSessionID, Subject: current.Subject, GrantID: &task.GrantID}
		result, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
		require.NoError(t, err)
		assert.False(t, result.ReconciliationOnly)
		assert.Equal(t, task.TaskID, result.TaskID)
		for _, field := range []string{"subject", "session"} {
			alias := request
			if field == "subject" {
				alias.Subject = strings.ToUpper(alias.Subject)
			} else {
				alias.AppSessionID = strings.ToUpper(alias.AppSessionID)
			}
			_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, alias)
			require.ErrorContains(t, err, "not_found", "caller binding must match exactly")
		}
		cancel := AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
			AppSessionID: current.AppSessionID, Subject: current.Subject, TaskID: task.TaskID, Reason: "user_requested"}
		_, err = f.execution.CancelTask(t.Context(), f.serviceID, "new-session", cancel)
		require.NoError(t, err)
		other := model.User{Username: "other-user", AffCode: "other-user", Status: common.UserStatusEnabled,
			Role: common.RoleCommonUser, Group: "default", AuthVersion: f.user.AuthVersion}
		require.NoError(t, f.db.Create(&other).Error)
		// Even a matching subject string cannot substitute for the derived user.
		require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", newDashboard.SID).Update("user_id", other.Id).Error)
		require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", current.AppSessionID).Update("user_id", other.Id).Error)
		_, err = f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
		require.ErrorContains(t, err, "not_found")
		_, err = f.execution.CancelTask(t.Context(), f.serviceID, "new-session", cancel)
		require.ErrorContains(t, err, "not_found")
	})
	for _, logout := range []bool{false, true} {
		for _, change := range []string{"service", "service_version", "service_expiry", "scope", "session_scope",
			"user_disabled", "auth_version", "dashboard_auth", "dashboard_version", "dashboard_expiry",
			"app_revoked", "app_expiry", "generation", "issuer", "installation", "installation_revoked", "user_policy",
			"task_user", "task_subject", "task_session", "task_run", "task_execution", "task_operation", "grant_missing"} {
			t.Run(change+map[bool]string{false: " active", true: " logout"}[logout], func(t *testing.T) {
				f := newAppExecutionFixture(t)
				task := f.acceptedTask(t)
				request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
					AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: &task.TaskID}
				if logout {
					f.logout(t, false)
				}
				var table any
				var column string
				var value any
				where, arg := "", any(nil)
				switch change {
				case "service", "service_expiry":
					table, where, arg = &model.AppServiceCredential{}, "credential_id = ?", f.serviceID.CredentialID
					column, value = "status", "revoked"
					if change == "service_expiry" {
						column, value = "expires_at", 1
					}
				case "service_version":
					f.serviceID.Version = "stale"
				case "scope":
					table, where, arg, column, value = &model.AppServiceCredentialBinding{}, "credential_id = ?", f.serviceID.CredentialID, "scopes", `["model.invoke"]`
				case "user_disabled", "auth_version":
					table, where, arg, column, value = &model.User{}, "id = ?", f.user.Id, "status", common.UserStatusDisabled
					if change == "auth_version" {
						column, value = "auth_version", f.user.AuthVersion+1
					}
				case "dashboard_auth", "dashboard_version", "dashboard_expiry":
					table, where, arg = &model.UserSession{}, "sid = ?", f.session.SID
					column, value = map[string]string{"dashboard_auth": "user_auth_version", "dashboard_version": "version", "dashboard_expiry": "expires_at"}[change], 1
				case "installation", "installation_revoked", "user_policy":
					table, where, arg, column, value = &model.AppInstallation{}, "installation_id = ?", f.installation.InstallationID, "status", "disabled"
					if change == "installation_revoked" {
						value = "revoked"
					}
					if change == "user_policy" {
						column, value = "allowed_user_policy", `{"groups":["other"]}`
					}
				case "grant_missing":
					require.NoError(t, f.db.Where("grant_id = ?", task.GrantID).Delete(&model.AppExecutionGrant{}).Error)
				default:
					table, where, arg = &model.AppPluginSession{}, "app_session_id = ?", task.AppSessionID
					column, value = map[string]string{"session_scope": "granted_scopes", "app_revoked": "revoked_at", "app_expiry": "upstream_expires_at", "generation": "generation", "issuer": "issuer"}[change], "changed"
					if change == "session_scope" {
						value = `["model.invoke"]`
					}
					if change == "app_revoked" || change == "app_expiry" {
						value = 1
					}
					if strings.HasPrefix(change, "task_") {
						table, where, arg = &model.AppTaskExecution{}, "task_id = ?", task.TaskID
						column = map[string]string{"task_user": "user_id", "task_subject": "subject", "task_session": "app_session_id",
							"task_run": "run_id", "task_execution": "execution_request_id", "task_operation": "operation"}[change]
						if change == "task_user" {
							value = f.user.Id + 100
						}
					}
				}
				if table != nil {
					require.NoError(t, f.db.Model(table).Where(where, arg).Update(column, value).Error)
				}
				_, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
				require.Error(t, err)
			})
		}
	}
	t.Run("read-only scope full facts and no side effects", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		task := f.acceptedTask(t)
		transport := &appControlNoHTTP{}
		previous := http.DefaultTransport
		http.DefaultTransport = transport
		t.Cleanup(func() { http.DefaultTransport = previous })
		source := &appArkSourceFixture{}
		f.execution.ArkSource = source
		require.NoError(t, f.db.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.serviceID.CredentialID).Update("scopes", `["task.read"]`).Error)
		require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", task.AppSessionID).Update("granted_scopes", `["task.read"]`).Error)
		request := AppTaskLookupRequest{RequestID: uuid.NewString(), AppKey: task.AppKey, AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: &task.TaskID}
		for _, state := range []string{"queued", "running", "succeeded", "failed", "cancelled"} {
			require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).Updates(map[string]any{
				"provider_state": state, "result_json": `{"error":{"code":"upstream_failed","message":"safe","headers":{"Authorization":"secret"}}}`,
			}).Error)
			var before model.AppTaskExecution
			require.NoError(t, f.db.First(&before, task.ID).Error)
			result, err := f.execution.LookupScopedTask(t.Context(), f.serviceID, request)
			require.NoError(t, err)
			raw, err := common.Marshal(result)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, common.Unmarshal(raw, &fields))
			assert.Len(t, fields, 13)
			assert.Equal(t, state, fields["status"])
			assert.Equal(t, state == "succeeded" || state == "failed" || state == "cancelled", fields["terminal"])
			assert.Equal(t, time.Unix(before.UpdatedAt, 0).UTC().Format(time.RFC3339Nano), fields["updated_at"])
			assert.NotContains(t, string(raw), "secret")
			assert.NotNil(t, fields["usage"])
			assert.NotEmpty(t, fields["artifacts"])
			var after model.AppTaskExecution
			require.NoError(t, f.db.First(&after, task.ID).Error)
			assert.Equal(t, before, after)
		}
		assert.Empty(t, source.queries)
		assert.Zero(t, transport.calls.Load())
		for _, table := range []any{&model.AppTaskSettlement{}, &model.AppTaskOutbox{}, &model.AppTaskCancelReplay{}} {
			var count int64
			require.NoError(t, f.db.Model(table).Count(&count).Error)
			assert.Zero(t, count)
		}
		var payer model.User
		require.NoError(t, f.db.First(&payer, f.user.Id).Error)
		assert.Equal(t, 1000000, payer.Quota)
	})
}

func TestAppTaskCancellationCurrentAuthorizationAndReplay(t *testing.T) {
	t.Run("known cancelled fact without invented time", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		task := f.acceptedTask(t)
		require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).UpdateColumn("provider_state", "cancelled").Error)
		request := AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
			AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: task.TaskID, Reason: "user_requested"}
		result, err := f.execution.CancelTask(t.Context(), f.serviceID, "known", request)
		require.NoError(t, err)
		assert.Equal(t, "cancelled", result.State)
		assert.Equal(t, "cancelled", result.ProviderStatus)
		assert.Nil(t, result.CancelledAt)
		raw, err := common.Marshal(result)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, common.Unmarshal(raw, &fields))
		assert.Len(t, fields, 5)
	})
	for _, invalidation := range []string{"logout", "scope", "session_scope", "service", "installation", "user"} {
		t.Run(invalidation, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			task := f.acceptedTask(t)
			transport := &appControlNoHTTP{}
			previous := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() { http.DefaultTransport = previous })
			source := &appArkSourceFixture{}
			f.execution.ArkSource = source
			request := AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
				AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: task.TaskID, Reason: "user_requested"}
			first, err := f.execution.CancelTask(t.Context(), f.serviceID, "cancel-once", request)
			require.NoError(t, err)
			assert.Equal(t, "not_cancellable", first.State)
			assert.Nil(t, first.CancelledAt)
			assert.Equal(t, "running", first.ProviderStatus)
			_, err = uuid.Parse(first.CancelRequestID)
			require.NoError(t, err)
			require.NoError(t, f.db.Model(&model.AppTaskExecution{}).Where("id = ?", task.ID).UpdateColumn("provider_state", "succeeded").Error)
			replay, err := NewAppExecutionService(f.db, f.options).CancelTask(t.Context(), f.serviceID, "cancel-once", request)
			require.NoError(t, err)
			assert.Equal(t, first, replay, "restart replay freezes the first response")
			changed := request
			changed.Reason = "another_reason"
			_, err = f.execution.CancelTask(t.Context(), f.serviceID, "cancel-once", changed)
			require.ErrorContains(t, err, "idempotency_conflict")
			switch invalidation {
			case "logout":
				f.logout(t, false)
			case "scope":
				require.NoError(t, f.db.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.serviceID.CredentialID).Update("scopes", `["task.read"]`).Error)
			case "session_scope":
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", task.AppSessionID).Update("granted_scopes", `["task.read"]`).Error)
			case "service":
				require.NoError(t, f.db.Model(&model.AppServiceCredential{}).Where("credential_id = ?", f.serviceID.CredentialID).Update("status", "revoked").Error)
			case "installation":
				require.NoError(t, f.db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).Update("status", "disabled").Error)
			case "user":
				require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", f.user.Id).Update("auth_version", 99).Error)
			}
			for _, key := range []string{"cancel-once", "new-key"} {
				_, err = f.execution.CancelTask(t.Context(), f.serviceID, key, request)
				require.Error(t, err, "authorization must precede replay")
				assert.NotContains(t, err.Error(), "idempotency")
			}
			var after model.AppTaskExecution
			require.NoError(t, f.db.First(&after, task.ID).Error)
			task.ProviderState = "succeeded"
			assert.Equal(t, task, after)
			var payer model.User
			require.NoError(t, f.db.First(&payer, f.user.Id).Error)
			assert.Equal(t, 1000000, payer.Quota)
			assert.Empty(t, source.queries)
			assert.Zero(t, transport.calls.Load())
			var count int64
			require.NoError(t, f.db.Model(&model.AppTaskCancelReplay{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			for _, table := range []any{&model.AppTaskSettlement{}, &model.AppTaskOutbox{}} {
				require.NoError(t, f.db.Model(table).Count(&count).Error)
				assert.Zero(t, count)
			}
		})
	}
	t.Run("contention and rollback", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		task := f.acceptedTask(t)
		request := AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: task.AppKey,
			AppSessionID: task.AppSessionID, Subject: task.Subject, TaskID: task.TaskID, Reason: "user_requested"}
		require.ErrorContains(t, f.db.Transaction(func(tx *gorm.DB) error {
			_, err := NewAppExecutionService(tx, f.options).CancelTask(t.Context(), f.serviceID, "rollback", request)
			if err != nil {
				return err
			}
			return errors.New("rollback probe")
		}), "rollback probe")
		var count int64
		require.NoError(t, f.db.Model(&model.AppTaskCancelReplay{}).Count(&count).Error)
		assert.Zero(t, count)
		start := make(chan struct{})
		results := make([]AppTaskCancelResult, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				<-start
				results[i], errs[i] = NewAppExecutionService(f.db, f.options).CancelTask(t.Context(), f.serviceID, "concurrent", request)
			})
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		assert.Equal(t, results[0], results[1])
		require.NoError(t, f.db.Model(&model.AppTaskCancelReplay{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
}

func TestAppTaskControlStrictDecoders(t *testing.T) {
	valid := `{"request_id":"` + uuid.NewString() + `","app_key":"app","app_session_id":"session","subject":"owner","task_id":"task","grant_id":null}`
	var request AppTaskLookupRequest
	require.NoError(t, DecodeAppTaskLookupRequest([]byte(valid), &request))
	for _, raw := range []string{
		strings.Replace(valid, `,"grant_id":null`, "", 1),
		strings.Replace(valid, `"grant_id":null`, `"grant_id":null,"grant_id":null`, 1),
		strings.Replace(valid, `"grant_id":null`, `"grant_id":null,"unknown":true`, 1),
		strings.Replace(valid, `"grant_id":null`, `"grant_id":"grant"`, 1),
		strings.Replace(valid, `"task_id":"task"`, `"task_id":null`, 1),
		strings.Replace(valid, `"task_id":"task"`, `"task_id":""`, 1),
		strings.Replace(valid, `"task_id":"task"`, `"task_id":"bad id"`, 1),
		strings.Replace(valid, `"task_id":"task"`, `"task_id":"bad\u0001id"`, 1),
	} {
		require.Error(t, DecodeAppTaskLookupRequest([]byte(raw), &request), raw)
	}
	var cancel AppTaskCancelRequest
	require.Error(t, DecodeAppTaskCancelRequest([]byte(`{"request_id":"bad"}`), &cancel))
	var ark AppArkImportLookupRequest
	require.Error(t, DecodeAppArkImportLookupRequest([]byte(`{"query_scope":null}`), &ark))
}

func TestArkImportLookupUsesDelegatedQueryScope(t *testing.T) {
	f := newAppExecutionFixture(t)
	source := &appArkSourceFixture{}
	f.execution.ArkSource = source
	start, end := f.options.Now().Add(-time.Hour), f.options.Now()
	delegations, err := common.Marshal(map[string]any{"delegations": []any{map[string]any{
		"installation_id": f.installation.InstallationID, "user_id": f.user.Id,
		"account_ref": "host-account", "project_id": "project-one", "start_at": start, "end_at": end,
	}}})
	require.NoError(t, err)
	version, err := PublishAppArkImportDelegations(t.Context(), f.db, f.publisher, delegations, f.options.Now())
	require.NoError(t, err)
	request := AppArkImportLookupRequest{RequestID: uuid.NewString(), AppKey: f.request.AppKey,
		AppSessionID: f.request.AppSessionID, Subject: f.request.Subject, TaskID: "cgt-historical-task",
		QueryScope: AppArkQueryScope{ProjectID: "project-one", StartAt: start.Add(10 * time.Minute), EndAt: end}}
	result, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.Equal(t, version, result.PolicyVersion)
	require.Len(t, source.queries, 1)
	assert.Equal(t, "host-account", source.queries[0].AccountRef)
	assert.Equal(t, request.QueryScope.StartAt, source.queries[0].StartAt)
	assert.Equal(t, request.QueryScope.EndAt, source.queries[0].EndAt)
	require.Len(t, result.Candidates, 1)
	assert.NotEmpty(t, result.EvidenceDigest)
	raw, err := common.Marshal(result)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(raw, []byte(f.credential.Credential)), "import evidence must exclude credentials")
	assert.NotContains(t, string(raw), "other-account-private")
	request.QueryScope.ProjectID = "other-project"
	_, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.ErrorContains(t, err, "scope_denied")
	assert.Len(t, source.queries, 1, "reject scope expansion before external lookup")

	task := f.acceptedTask(t)
	request.TaskID = task.TaskID
	_, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.Error(t, err, "a registered Task does not substitute for import delegation")
	assert.Len(t, source.queries, 1)
}

func TestAppArkImportEvidenceBoundary(t *testing.T) {
	f := newAppExecutionFixture(t)
	start, end := f.options.Now().Add(-time.Hour), f.options.Now()
	delegations, err := common.Marshal(AppArkImportDelegations{Delegations: []AppArkImportDelegation{{
		InstallationID: f.installation.InstallationID, UserID: f.user.Id,
		AccountRef: "host-account", ProjectID: "project-one", StartAt: start, EndAt: end,
	}}})
	require.NoError(t, err)
	_, err = PublishAppArkImportDelegations(t.Context(), f.db, f.publisher, delegations, f.options.Now())
	require.NoError(t, err)
	request := AppArkImportLookupRequest{RequestID: uuid.NewString(), AppKey: f.request.AppKey,
		AppSessionID: f.request.AppSessionID, Subject: f.request.Subject, TaskID: "cgt-historical-task",
		QueryScope: AppArkQueryScope{ProjectID: "project-one", StartAt: start.Add(time.Minute), EndAt: end}}
	_, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.ErrorContains(t, err, "service_unavailable")
	require.NoError(t, f.db.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.serviceID.CredentialID).Update("scopes", `["task.import"]`).Error)
	require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", f.request.AppSessionID).Update("granted_scopes", `["task.import"]`).Error)
	valid := AppArkTaskEvidence{TaskID: request.TaskID, AccountRef: "host-account", ProjectID: "project-one",
		CreatedAt: start.Add(2 * time.Minute), RequestJSON: `{"model":"seedance","prompt":"original","nested":{"api_key":"secret","headers":{"Authorization":"secret"},"cookie":"secret","private_key":"secret","token":"secret","api_token":"secret","X-Api-Key":"secret","seed":7},"nullable":null}`}
	source := &appArkSourceFixture{evidence: []AppArkTaskEvidence{valid}}
	f.execution.ArkSource = source
	result, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	raw, err := common.Marshal(result)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, common.Unmarshal(raw, &fields))
	assert.Len(t, fields, 6)
	assert.NotNil(t, fields["authorization"])
	authorization, ok := fields["authorization"].([]any)
	require.True(t, ok)
	require.Len(t, authorization, 1)
	assert.Equal(t, map[string]any{"task_id": request.TaskID, "account_ref": "host-account", "project_id": "project-one",
		"start_at": request.QueryScope.StartAt.Format(time.RFC3339Nano), "end_at": request.QueryScope.EndAt.Format(time.RFC3339Nano)}, authorization[0])
	assert.NotNil(t, fields["missing_fields"])
	assert.Contains(t, result.MissingFields, "candidates[0].model_version")
	assert.Contains(t, result.MissingFields, "candidates[0].gateway_version")
	assert.Contains(t, result.MissingFields, "candidates[0].adapter_version")
	assert.Contains(t, result.MissingFields, "candidates[0].source")
	assert.NotNil(t, fields["warnings"])
	assert.NotContains(t, string(raw), "secret")
	assert.Contains(t, result.Candidates[0].RequestJSON, "original")
	assert.Contains(t, result.Candidates[0].RequestJSON, `"seed":7`)
	known := valid
	known.Source, known.ModelVersion, known.GatewayVersion, known.AdapterVersion = "ark_export", "model-v1", "gateway-v2", "adapter-v3"
	source.evidence = []AppArkTaskEvidence{known}
	result, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.Empty(t, result.MissingFields)
	assert.Equal(t, known.Source, result.Candidates[0].Source)
	assert.Equal(t, known.ModelVersion, result.Candidates[0].ModelVersion)
	assert.Equal(t, known.GatewayVersion, result.Candidates[0].GatewayVersion)
	assert.Equal(t, known.AdapterVersion, result.Candidates[0].AdapterVersion)
	for _, field := range []string{"task", "account", "project", "time", "no_account", "no_time"} {
		bad := valid
		switch field {
		case "task":
			bad.TaskID = "cgt-another"
		case "account":
			bad.AccountRef = "another-account"
		case "project":
			bad.ProjectID = "another-project"
		case "time":
			bad.CreatedAt = start
		case "no_account":
			bad.AccountRef = ""
		case "no_time":
			bad.CreatedAt = time.Time{}
		}
		source.evidence = []AppArkTaskEvidence{bad}
		_, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)
		require.ErrorContains(t, err, "not_found", field)
	}
	for _, invalid := range []string{`{"x":1,"x":2}`, `{"x":1,"\u0078":2}`, `{"x":NaN}`, `{"x":1e999}`, `[]`, `{"x":"` + strings.Repeat("a", 65536) + `"}`} {
		bad := valid
		bad.RequestJSON = invalid
		source.evidence = []AppArkTaskEvidence{bad}
		_, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)
		require.ErrorContains(t, err, "invalid_evidence")
	}
	missing := valid
	missing.RequestJSON = ""
	source.evidence = []AppArkTaskEvidence{valid, missing}
	result, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.Len(t, result.Candidates, 2)
	assert.NotEmpty(t, result.MissingFields)
	assert.Contains(t, result.Warnings, "human_selection_required")
	again, err := f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.Equal(t, result.EvidenceDigest, again.EvidenceDigest)
	before := len(source.queries)
	expanded := request
	expanded.QueryScope.StartAt = start.Add(-time.Second)
	_, err = f.execution.LookupArkImport(t.Context(), f.serviceID, expanded)
	require.ErrorContains(t, err, "scope_denied")
	assert.Len(t, source.queries, before)
	source.err = errors.New("Authorization: secret")
	_, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.ErrorContains(t, err, "service_unavailable")
	assert.NotContains(t, err.Error(), "secret")
	f.logout(t, false)
	before = len(source.queries)
	_, err = f.execution.LookupArkImport(t.Context(), f.serviceID, request)
	require.Error(t, err)
	assert.Len(t, source.queries, before, "no logout exception for import")
}
