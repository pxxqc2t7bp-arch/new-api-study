package middleware

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	channelconstraint "github.com/QuantumNous/new-api/dto"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
)

var arkAssetReferenceFields = map[string]struct{}{
	"image":           {},
	"images":          {},
	"input_reference": {},
	"image_url":       {},
	"video_url":       {},
	"audio_url":       {},
	"url":             {},
}

var arkAssetContainerFields = map[string]struct{}{
	"content":  {},
	"input":    {},
	"metadata": {},
}

func appendArkAssetAffinityFilter(c *gin.Context, constraints *channelconstraint.ChannelConstraints) error {
	if c == nil || constraints == nil || c.Request == nil {
		return nil
	}
	if c.Request.Method != http.MethodPost &&
		c.Request.Method != http.MethodPut &&
		c.Request.Method != http.MethodPatch {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(c.Request.Header.Get("Content-Type")), "application/json") {
		return nil
	}

	var body any
	if err := common.UnmarshalBodyReusable(c, &body); err != nil {
		return err
	}
	if !containsArkAssetReference(body) {
		return nil
	}
	constraints.AddFilter(channelconstraint.ChannelFilter{
		Kind:           channelconstraint.FilterRoutingAccount,
		RoutingAccount: kitdto.RoutingAccountCXY,
	})
	return nil
}

func containsArkAssetReference(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if _, ok := arkAssetReferenceFields[normalized]; ok && arkAssetReferenceValue(nested) {
				return true
			}
			if _, ok := arkAssetContainerFields[normalized]; ok && containsArkAssetReference(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsArkAssetReference(nested) {
				return true
			}
		}
	}
	return false
}

func arkAssetReferenceValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(typed)), "asset://")
	case map[string]any:
		for key, nested := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if _, ok := arkAssetReferenceFields[normalized]; ok && arkAssetReferenceValue(nested) {
				return true
			}
			if _, ok := arkAssetContainerFields[normalized]; ok && containsArkAssetReference(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if arkAssetReferenceValue(nested) {
				return true
			}
		}
	}
	return false
}
