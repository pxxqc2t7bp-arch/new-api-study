package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	arkResponsesHardInputItemLimit     = 1000
	responsesInputItemSoftLimitHeader  = "X-NewAPI-Responses-Input-Item-Soft-Limit"
	responsesInputItemLimitContextKey  = "relay_responses_input_item_limit_control"
	responsesInputItemLimitSourceArk   = "ark_default"
	responsesInputItemLimitSourceOptIn = "client_opt_in"
)

type responsesInputItemLimitControl struct {
	values  []string
	present bool
}

func resolveResponsesInputItemLimit(raw string) (limit int, source string, err error) {
	if raw == "" {
		return arkResponsesHardInputItemLimit, responsesInputItemLimitSourceArk, nil
	}

	limit, err = strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > arkResponsesHardInputItemLimit {
		return 0, "", fmt.Errorf(
			"%s must be an integer from 1 through %d",
			responsesInputItemSoftLimitHeader,
			arkResponsesHardInputItemLimit,
		)
	}
	return limit, responsesInputItemLimitSourceOptIn, nil
}

func isNativeArkResponsesURL(rawURL string) bool {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	host := strings.TrimSuffix(strings.ToLower(parsedURL.Hostname()), ".")
	labels := strings.Split(host, ".")
	path := strings.TrimSuffix(parsedURL.EscapedPath(), "/")
	return len(labels) == 4 &&
		labels[0] == "ark" &&
		labels[1] != "" &&
		labels[2] == "volces" &&
		labels[3] == "com" &&
		strings.HasSuffix(path, "/responses")
}

func countResponsesInputItems(raw json.RawMessage) (count int, isArray bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return 0, false, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := decoder.Token(); err != nil {
		return 0, true, err
	}

	var item json.RawMessage
	for decoder.More() {
		if err := decoder.Decode(&item); err != nil {
			return count, true, err
		}
		count++
		item = item[:0]
	}
	if _, err := decoder.Token(); err != nil {
		return count, true, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected JSON token after input array")
		}
		return count, true, err
	}
	return count, true, nil
}

func enforceArkResponsesInputItemLimit(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	requestURL string,
	request *dto.OpenAIResponsesRequest,
) *types.NewAPIError {
	controlValue, _ := c.Get(responsesInputItemLimitContextKey)
	control, captured := controlValue.(responsesInputItemLimitControl)
	if c.Request != nil {
		for key, values := range c.Request.Header {
			if strings.EqualFold(key, responsesInputItemSoftLimitHeader) {
				if !captured {
					control.present = true
					control.values = append(control.values, values...)
				}
				delete(c.Request.Header, key)
			}
		}
	}
	if !captured {
		c.Set(responsesInputItemLimitContextKey, control)
	}

	if !isNativeArkResponsesURL(requestURL) {
		return nil
	}

	var limit int
	var source string
	var err error
	if control.present && (len(control.values) != 1 || control.values[0] == "") {
		err = fmt.Errorf(
			"%s must be an integer from 1 through %d",
			responsesInputItemSoftLimitHeader,
			arkResponsesHardInputItemLimit,
		)
	} else {
		rawLimit := ""
		if control.present {
			rawLimit = control.values[0]
		}
		limit, source, err = resolveResponsesInputItemLimit(rawLimit)
	}
	if err != nil {
		return types.WithOpenAIError(types.OpenAIError{
			Message: err.Error(),
			Type:    "invalid_request_error",
			Param:   responsesInputItemSoftLimitHeader,
			Code:    string(types.ErrorCodeInvalidRequest),
		}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	count, isArray, err := countResponsesInputItems(request.Input)
	if err != nil {
		return types.WithOpenAIError(types.OpenAIError{
			Message: fmt.Sprintf("Invalid Responses input array: %v", err),
			Type:    "invalid_request_error",
			Param:   "input",
			Code:    string(types.ErrorCodeInvalidRequest),
		}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if !isArray || count <= limit {
		return nil
	}

	channelID := 0
	if info.ChannelMeta != nil {
		channelID = info.ChannelMeta.ChannelId
	}
	logger.LogWarn(
		c,
		"Ark Responses input item limit exceeded: channel_id=%d model=%s item_count=%d limit=%d source=%s",
		channelID,
		request.Model,
		count,
		limit,
		source,
	)
	return types.WithOpenAIError(types.OpenAIError{
		Message: fmt.Sprintf(
			"Responses input contains %d items; the configured maximum is %d. Compact the conversation and retry.",
			count,
			limit,
		),
		Type:  "invalid_request_error",
		Param: "input",
		Code:  "context_length_exceeded",
	}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
}
