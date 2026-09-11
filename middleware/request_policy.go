package middleware

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const (
	HeaderRoutingStrategy  = "X-NewAPI-Routing-Strategy"
	HeaderConversionPolicy = "X-NewAPI-Conversion-Policy"
)

func OrdinaryRequestPolicy() gin.HandlerFunc {
	return func(c *gin.Context) {
		routing, status, err := resolveRoutingStrategy(c)
		if err != nil {
			abortWithOpenAiMessage(c, status, err.Error(), requestPolicyErrorCode(status))
			return
		}
		conversion, status, err := resolveConversionPolicy(c)
		if err != nil {
			abortWithOpenAiMessage(c, status, err.Error(), requestPolicyErrorCode(status))
			return
		}

		common.SetContextKey(c, constant.ContextKeyRoutingStrategy, routing)
		common.SetContextKey(c, constant.ContextKeyConversionPolicy, conversion)
		c.Next()
	}
}

func resolveRoutingStrategy(c *gin.Context) (hosttypes.RoutingStrategy, int, error) {
	allowed := common.GetContextKeyStringSlice(c, constant.ContextKeyTokenAllowedRoutingStrategies)
	allowedSet := make(map[hosttypes.RoutingStrategy]struct{}, len(allowed))
	if len(allowed) == 0 {
		allowedSet[hosttypes.RoutingStrategyStable] = struct{}{}
	} else {
		for _, value := range allowed {
			strategy, ok := hosttypes.ParseRoutingStrategy(value)
			if !ok {
				allowedSet = map[hosttypes.RoutingStrategy]struct{}{hosttypes.RoutingStrategyStable: {}}
				break
			}
			allowedSet[strategy] = struct{}{}
		}
	}

	requested := c.GetHeader(HeaderRoutingStrategy)
	if requested != "" {
		strategy, ok := hosttypes.ParseRoutingStrategy(requested)
		if !ok {
			return "", http.StatusBadRequest, fmt.Errorf("unsupported routing strategy %q", requested)
		}
		if _, ok := allowedSet[strategy]; !ok {
			return "", http.StatusForbidden, fmt.Errorf("routing strategy %q is not authorized for this token", strategy)
		}
		return strategy, 0, nil
	}

	value := common.GetContextKeyString(c, constant.ContextKeyTokenDefaultRoutingStrategy)
	if value == "" {
		return hosttypes.RoutingStrategyStable, 0, nil
	}
	strategy, ok := hosttypes.ParseRoutingStrategy(value)
	if !ok {
		return hosttypes.RoutingStrategyStable, 0, nil
	}
	if _, ok := allowedSet[strategy]; !ok {
		return hosttypes.RoutingStrategyStable, 0, nil
	}
	return strategy, 0, nil
}

func resolveConversionPolicy(c *gin.Context) (types.ConversionLossPolicy, int, error) {
	authorized := common.GetContextKeyBool(c, constant.ContextKeyTokenAllowLossyConversion)
	requested := types.ConversionLossPolicy(c.GetHeader(HeaderConversionPolicy))
	if requested != "" {
		if !isRecognizedConversionPolicy(requested) {
			return "", http.StatusBadRequest, fmt.Errorf("unsupported conversion policy %q", requested)
		}
		policy, err := service.ResolveOrdinaryConversionPolicy(requested, authorized)
		if err != nil {
			return "", http.StatusForbidden, err
		}
		return policy, 0, nil
	}

	requested = types.ConversionLossPolicy(common.GetContextKeyString(c, constant.ContextKeyTokenDefaultConversionPolicy))
	if !isRecognizedConversionPolicy(requested) {
		return types.ConversionLossPolicyStrict, 0, nil
	}
	policy, err := service.ResolveOrdinaryConversionPolicy(requested, authorized)
	if err != nil {
		return types.ConversionLossPolicyStrict, 0, nil
	}
	return policy, 0, nil
}

func isRecognizedConversionPolicy(policy types.ConversionLossPolicy) bool {
	switch policy {
	case "", types.ConversionLossPolicyStrict, types.ConversionLossPolicySafe, types.ConversionLossPolicyAllow:
		return true
	default:
		return false
	}
}

func requestPolicyErrorCode(status int) types.ErrorCode {
	if status == http.StatusForbidden {
		return types.ErrorCodeAccessDenied
	}
	return types.ErrorCodeInvalidRequest
}
