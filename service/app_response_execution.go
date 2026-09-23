package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	hosttypes "github.com/QuantumNous/new-api/types"
	"golang.org/x/net/http/httpguts"
	"gorm.io/gorm"
)

type AppResponseExecutionResult struct {
	Execution model.AppTaskExecution
	Result    model.AppResponseResult
	Replayed  bool
}

type appResponseDispatch struct {
	execution model.AppTaskExecution
	replay    *model.AppResponseResult
	candidate model.AppExecutionModel
	request   *http.Request
}

func (s *AppExecutionService) ExecuteAppResponse(ctx context.Context, subject hosttypes.AppRelaySubject,
	raw []byte) (AppResponseExecutionResult, error) {
	var dispatch appResponseDispatch
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		grant, user, err := s.appRelayAuthorityTx(tx, subject)
		if err != nil {
			return err
		}
		candidates, prepared, err := appNativeResponseCandidates(grant, subject, raw)
		if err != nil {
			return err
		}
		submissionHash, err := appPluginHash([]any{subject.Protocol, prepared.Outbound})
		if err != nil {
			return err
		}
		if submissionHash != subject.SubmissionHash {
			return appAuthError("scope_denied")
		}
		execution, replay, found, err := model.LookupAppResponseExecutionTx(
			tx, grant.GrantID, subject.SubmissionHash,
		)
		if err != nil {
			return err
		}
		if found {
			dispatch.execution, dispatch.replay = execution, replay
			return nil
		}

		var candidate model.AppExecutionModel
		var channel model.Channel
		for _, current := range candidates {
			selected, validationErr := validateNativeAppRelayChannelTx(
				tx, grant, user, current,
				[]taskdto.ChannelFilter{{
					Kind: taskdto.FilterRequestPath, RequestPath: "/v1/responses",
				}},
				s.options.DerivationKey,
			)
			if validationErr == nil {
				candidate, channel = current, selected
				break
			}
			var authErr *AppPluginAuthError
			if !errors.As(validationErr, &authErr) || authErr.Code == "service_unavailable" {
				return appExecutionBoundaryError(validationErr)
			}
		}
		if candidate.ChannelID == 0 {
			return appAuthError("model_not_supported")
		}
		estimatedInputTokens := min(max(len(prepared.Outbound), 1), constant.MaxTokensLimit)
		facts := AppResponseRequestFacts{
			EstimatedInputTokens: estimatedInputTokens,
			MaxOutputTokens:      prepared.MaxOutputTokens,
		}
		quote, err := ComputeAppResponseMaximumQuota(candidate, facts, s.options.Now().Unix())
		if err != nil {
			return err
		}
		factsJSON, err := common.Marshal(facts)
		if err != nil {
			return err
		}
		requestURL, err := buildAppResponseURL(candidate.BaseURL)
		if err != nil {
			return err
		}
		organization := ""
		if channel.OpenAIOrganization != nil {
			organization = *channel.OpenAIOrganization
		}
		request, err := prepareAppResponseHTTPRequest(
			context.WithoutCancel(ctx), requestURL, prepared.Outbound, channel.Key, organization,
		)
		if err != nil {
			return err
		}
		execution, replay, won, err := model.ClaimAppResponseExecutionTx(tx, grant.GrantID,
			model.AppTaskExecution{
				GrantID: grant.GrantID, AppKey: grant.AppKey,
				InstallationID: grant.InstallationID, AppSessionID: grant.AppSessionID,
				Subject: grant.Subject, UserID: grant.UserID, RunID: grant.RunID,
				ExecutionRequestID: grant.ExecutionRequestID, Operation: grant.Operation,
				SubmissionHash: subject.SubmissionHash, PublicModel: candidate.PublicModel,
				ActualModel: candidate.ActualModel, ActualGroup: candidate.Group,
				ChannelID: candidate.ChannelID, ConnectionDigest: candidate.ConnectionDigest,
				CredentialDigest: candidate.CredentialDigest,
				CredentialIndex:  candidate.CredentialIndex, Protocol: candidate.Protocol,
				FundingSource: grant.FundingSource, ReservedQuota: int64(quote.Quota),
				RequestFactsJSON: string(factsJSON),
			}, s.options.Now())
		if err != nil {
			return err
		}
		if !won && replay == nil {
			return appAuthError("execution_outcome_unknown")
		}
		dispatch.execution, dispatch.replay = execution, replay
		dispatch.candidate, dispatch.request = candidate, request
		return nil
	})
	if err != nil {
		return AppResponseExecutionResult{}, appResponseExecutionError(err)
	}
	if dispatch.replay != nil {
		return AppResponseExecutionResult{
			Execution: dispatch.execution, Result: *dispatch.replay, Replayed: true,
		}, nil
	}

	sendContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), s.appResponseTimeout(),
	)
	defer cancel()
	request := dispatch.request.WithContext(sendContext)

	roundTripper, closeIdle := s.appResponseRoundTripper()
	if closeIdle != nil {
		defer closeIdle()
	}
	response, err := roundTripper.RoundTrip(request)
	if err != nil {
		return AppResponseExecutionResult{}, s.markAppResponseUnknown(
			ctx, dispatch.execution.ID, err,
		)
	}
	if response == nil || response.Body == nil {
		return AppResponseExecutionResult{}, s.markAppResponseUnknown(
			ctx, dispatch.execution.ID, errors.New("empty upstream response"),
		)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, appResponseMaxBodyBytes+1))
	if err != nil || len(responseBody) > appResponseMaxBodyBytes {
		if err == nil {
			err = errors.New("upstream response too large")
		}
		return AppResponseExecutionResult{}, s.markAppResponseUnknown(
			ctx, dispatch.execution.ID, err,
		)
	}
	parsed, err := ParseAppResponse(
		response.StatusCode, response.Header, responseBody, dispatch.candidate.ActualModel,
	)
	if err != nil {
		return AppResponseExecutionResult{}, s.markAppResponseUnknown(
			ctx, dispatch.execution.ID, err,
		)
	}
	finalQuota := 0
	if !parsed.Rejected {
		var facts AppResponseRequestFacts
		if common.UnmarshalJsonStr(dispatch.execution.RequestFactsJSON, &facts) != nil {
			return AppResponseExecutionResult{}, s.markAppResponseUnknown(
				ctx, dispatch.execution.ID, errors.New("invalid request facts"),
			)
		}
		final, err := ComputeAppResponseFinalQuota(
			dispatch.candidate, facts, parsed.Usage,
			int(dispatch.execution.ReservedQuota), dispatch.execution.PricedAt,
		)
		if err != nil {
			return AppResponseExecutionResult{}, s.markAppResponseUnknown(
				ctx, dispatch.execution.ID, err,
			)
		}
		finalQuota = final.Quota
	}
	providerStatus := parsed.ProviderStatus
	if parsed.Rejected {
		providerStatus = "rejected"
	}
	terminal := model.AppResponseTerminal{
		Rejected: parsed.Rejected, HTTPStatus: parsed.HTTPStatus,
		SafeHeadersJSON: parsed.SafeHeadersJSON, Body: parsed.Body,
		BodySHA256: parsed.BodySHA256, ResponseID: parsed.ResponseID,
		ProviderModel: parsed.ProviderModel, ProviderStatus: providerStatus,
		RawUsageJSON:        parsed.RawUsageJSON,
		NormalizedUsageJSON: parsed.NormalizedUsageJSON,
		FinalQuota:          int64(finalQuota),
	}
	persistContext, persistCancel := context.WithTimeout(
		context.WithoutCancel(ctx), 5*time.Second,
	)
	defer persistCancel()
	var committed model.AppResponseResult
	err = model.RunAppPluginTransaction(s.db.WithContext(persistContext), func(tx *gorm.DB) error {
		var finalizeErr error
		committed, finalizeErr = model.FinalizeAppResponseTx(
			tx, dispatch.execution.ID, terminal, s.options.Now(),
		)
		return finalizeErr
	})
	if err != nil {
		markErr := s.markAppResponseUnknown(ctx, dispatch.execution.ID, err)
		var authErr *AppPluginAuthError
		if errors.As(markErr, &authErr) && authErr.Code == "service_unavailable" {
			return AppResponseExecutionResult{}, markErr
		}
		return AppResponseExecutionResult{}, appAuthError("service_unavailable")
	}
	return AppResponseExecutionResult{
		Execution: dispatch.execution, Result: committed,
	}, nil
}

func prepareAppResponseHTTPRequest(ctx context.Context, requestURL string, outbound []byte,
	credential, organization string) (*http.Request, error) {
	headers := http.Header{
		"Accept":        {"application/json"},
		"Authorization": {"Bearer " + credential},
		"Content-Type":  {"application/json"},
	}
	if organization != "" {
		headers.Set("OpenAI-Organization", organization)
	}
	for _, values := range headers {
		for _, value := range values {
			if len(value) > 8*1024 || !httpguts.ValidHeaderFieldValue(value) {
				return nil, appAuthError("channel_changed")
			}
		}
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, requestURL, bytes.NewReader(outbound),
	)
	if err != nil {
		return nil, appAuthError("channel_changed")
	}
	request.GetBody = nil
	request.Close = true
	request.ContentLength = int64(len(outbound))
	request.Header = headers
	return request, nil
}

func appResponseExecutionError(err error) error {
	if err == nil {
		return nil
	}
	var authError *AppPluginAuthError
	if errors.As(err, &authError) {
		return err
	}
	switch err.Error() {
	case "scope_denied", "invalid_grant", "execution_grant_expired",
		"idempotency_conflict", "execution_outcome_unknown", "insufficient_quota",
		"identity_inactive", "funding_period_changed", "wallet_accounting_unavailable",
		"pricing_not_supported":
		return appAuthError(err.Error())
	default:
		return appExecutionBoundaryError(err)
	}
}

func appNativeResponseCandidates(grant model.AppExecutionGrant, subject hosttypes.AppRelaySubject,
	raw []byte) ([]model.AppExecutionModel, AppPreparedResponseRequest, error) {
	if subject.ExecutionKind != model.AppExecutionModelKindNativeResponse ||
		subject.Protocol != "openai_responses" ||
		(subject.ChannelID == 0) != (subject.Group == "") {
		return nil, AppPreparedResponseRequest{}, appAuthError("scope_denied")
	}
	var candidates []model.AppExecutionModel
	if common.UnmarshalJsonStr(grant.ModelsJSON, &candidates) != nil {
		return nil, AppPreparedResponseRequest{}, appAuthError("invalid_grant")
	}
	matches := make([]model.AppExecutionModel, 0, len(candidates))
	var prepared AppPreparedResponseRequest
	for _, candidate := range candidates {
		if candidate.ExecutionKind == subject.ExecutionKind &&
			candidate.PublicModel == subject.PublicModel &&
			candidate.Protocol == subject.Protocol &&
			(subject.ChannelID == 0 ||
				candidate.ChannelID == subject.ChannelID && candidate.Group == subject.Group) {
			current, err := PrepareAppResponseRequest(raw, candidate)
			if err != nil {
				return nil, AppPreparedResponseRequest{}, err
			}
			if len(matches) != 0 && !bytes.Equal(prepared.Outbound, current.Outbound) {
				return nil, AppPreparedResponseRequest{}, appAuthError("invalid_grant")
			}
			prepared = current
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return nil, AppPreparedResponseRequest{}, appAuthError("scope_denied")
	}
	return matches, prepared, nil
}

func buildAppResponseURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || !validNativeAppBaseURL(base) {
		return "", appAuthError("channel_changed")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		path += "/responses"
	} else {
		path += "/v1/responses"
	}
	parsed.Path, parsed.RawPath = path, ""
	return parsed.String(), nil
}

func (s *AppExecutionService) appResponseRoundTripper() (http.RoundTripper, func()) {
	if s.ResponseRoundTripper != nil {
		return s.ResponseRoundTripper, nil
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  s.appResponseTimeout(),
		MaxResponseHeaderBytes: appResponseMaxBodyBytes,
	}
	return transport, transport.CloseIdleConnections
}

func (s *AppExecutionService) appResponseTimeout() time.Duration {
	if s.ResponseTimeout > 0 {
		return s.ResponseTimeout
	}
	return 30 * time.Second
}

func (s *AppExecutionService) markAppResponseUnknown(ctx context.Context,
	executionID int64, _ error) error {
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := model.RunAppPluginTransaction(s.db.WithContext(persistContext), func(tx *gorm.DB) error {
		return model.MarkAppResponseOutcomeUnknownTx(tx, executionID, s.options.Now())
	})
	if err != nil {
		return appExecutionBoundaryError(err)
	}
	return appAuthError("execution_outcome_unknown")
}
