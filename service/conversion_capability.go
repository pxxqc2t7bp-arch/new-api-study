package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type OrdinaryConversionFeature string

const (
	ConversionFeatureModel                OrdinaryConversionFeature = "model"
	ConversionFeatureSystemInstructions   OrdinaryConversionFeature = "system_instructions"
	ConversionFeatureTextInput            OrdinaryConversionFeature = "text_input"
	ConversionFeatureMultimodalInput      OrdinaryConversionFeature = "multimodal_input"
	ConversionFeatureOptionalScalars      OrdinaryConversionFeature = "optional_scalars"
	ConversionFeatureFunctionTools        OrdinaryConversionFeature = "function_tools"
	ConversionFeatureToolChoice           OrdinaryConversionFeature = "tool_choice"
	ConversionFeatureToolCallsAndResults  OrdinaryConversionFeature = "tool_calls_and_results"
	ConversionFeatureParallelToolCalls    OrdinaryConversionFeature = "parallel_tool_calls"
	ConversionFeatureStructuredOutput     OrdinaryConversionFeature = "structured_output"
	ConversionFeatureUsage                OrdinaryConversionFeature = "usage"
	ConversionFeatureTerminalStreamEvents OrdinaryConversionFeature = "terminal_stream_events"
	ConversionFeatureErrors               OrdinaryConversionFeature = "errors"
)

type OrdinaryConversionLoss struct {
	Code        string                    `json:"code"`
	Feature     OrdinaryConversionFeature `json:"feature"`
	Description string                    `json:"description"`
}

type OrdinaryConversionCapability struct {
	From       types.RelayFormat                 `json:"from"`
	To         types.RelayFormat                 `json:"to"`
	Converter  string                            `json:"converter"`
	Quality    relayconvert.TextConverterQuality `json:"quality"`
	Supported  []OrdinaryConversionFeature       `json:"supported"`
	Losses     []OrdinaryConversionLoss          `json:"losses"`
	Rejections []OrdinaryConversionLoss          `json:"rejections"`
}

var ordinaryConversionFormats = []types.RelayFormat{
	types.RelayFormatOpenAI,
	types.RelayFormatOpenAIResponses,
	types.RelayFormatClaude,
	types.RelayFormatGemini,
}

var ordinaryConversionSupported = []OrdinaryConversionFeature{
	ConversionFeatureModel,
	ConversionFeatureSystemInstructions,
	ConversionFeatureTextInput,
	ConversionFeatureMultimodalInput,
	ConversionFeatureOptionalScalars,
	ConversionFeatureFunctionTools,
	ConversionFeatureToolChoice,
	ConversionFeatureToolCallsAndResults,
	ConversionFeatureUsage,
	ConversionFeatureTerminalStreamEvents,
	ConversionFeatureErrors,
}

var ordinaryConversionLosses = []OrdinaryConversionLoss{
	{
		Code:        "provider_metadata_reduced",
		Feature:     ConversionFeatureMultimodalInput,
		Description: "provider-specific presentation metadata may be normalized to the target protocol",
	},
}

var ordinaryConversionRejections = []OrdinaryConversionLoss{
	{
		Code:        "vendor_specific_tool_unsupported",
		Feature:     ConversionFeatureFunctionTools,
		Description: "vendor-specific hosted tools without a verified target mapping are rejected",
	},
}

func OrdinaryConversionCapabilities() []OrdinaryConversionCapability {
	capabilities := make([]OrdinaryConversionCapability, 0, 12)
	for _, from := range ordinaryConversionFormats {
		for _, to := range ordinaryConversionFormats {
			if from == to {
				continue
			}
			spec, ok := relayconvert.LookupTextConverterRoute(from, to)
			if !ok {
				continue
			}
			capabilities = append(capabilities, ordinaryConversionCapability(spec))
		}
	}
	return capabilities
}

func LookupOrdinaryConversionCapability(from types.RelayFormat, to types.RelayFormat) (OrdinaryConversionCapability, bool) {
	spec, ok := relayconvert.LookupTextConverterRoute(from, to)
	if !ok {
		return OrdinaryConversionCapability{}, false
	}
	return ordinaryConversionCapability(spec), true
}

func ResolveOrdinaryConversionPolicy(requested types.ConversionLossPolicy, callerAuthorized bool) (types.ConversionLossPolicy, error) {
	switch requested {
	case "", types.ConversionLossPolicyStrict:
		return types.ConversionLossPolicyStrict, nil
	case types.ConversionLossPolicySafe, types.ConversionLossPolicyAllow:
		if !callerAuthorized {
			return "", fmt.Errorf("conversion policy %q requires caller authorization", requested)
		}
		return requested, nil
	default:
		return "", fmt.Errorf("unsupported conversion policy %q", requested)
	}
}

func ordinaryConversionCapability(spec relayconvert.TextConverterSpec) OrdinaryConversionCapability {
	supported := append([]OrdinaryConversionFeature(nil), ordinaryConversionSupported...)
	losses := append([]OrdinaryConversionLoss(nil), ordinaryConversionLosses...)
	rejections := append([]OrdinaryConversionLoss(nil), ordinaryConversionRejections...)

	if supportsStructuredOutput(spec.From, spec.To) {
		supported = append(supported, ConversionFeatureStructuredOutput)
	} else {
		losses = append(losses, OrdinaryConversionLoss{
			Code:        "structured_output_unsupported",
			Feature:     ConversionFeatureStructuredOutput,
			Description: "the target conversion does not preserve the source structured-output dialect",
		})
	}
	if spec.From != types.RelayFormatGemini && spec.To != types.RelayFormatGemini {
		supported = append(supported, ConversionFeatureParallelToolCalls)
	} else {
		losses = append(losses, OrdinaryConversionLoss{
			Code:        "parallel_tool_calls_unsupported",
			Feature:     ConversionFeatureParallelToolCalls,
			Description: "Gemini generateContent does not expose an equivalent parallel-tool-call control",
		})
	}
	if spec.From == types.RelayFormatClaude || spec.From == types.RelayFormatGemini {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        "encrypted_reasoning_unsupported",
			Feature:     ConversionFeatureTextInput,
			Description: "encrypted or signed provider reasoning state is not portable across protocols",
		})
	}
	if spec.From == types.RelayFormatOpenAIResponses || spec.From == types.RelayFormatClaude || spec.From == types.RelayFormatGemini {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        "session_reference_unsupported",
			Feature:     ConversionFeatureTextInput,
			Description: "provider-owned conversation, continuation, and session references are not portable across protocols",
		})
	}

	return OrdinaryConversionCapability{
		From:       spec.From,
		To:         spec.To,
		Converter:  spec.ID,
		Quality:    spec.Quality,
		Supported:  supported,
		Losses:     losses,
		Rejections: rejections,
	}
}

func supportsStructuredOutput(from types.RelayFormat, to types.RelayFormat) bool {
	if from != types.RelayFormatOpenAI && from != types.RelayFormatOpenAIResponses {
		return false
	}
	return to == types.RelayFormatOpenAI ||
		to == types.RelayFormatOpenAIResponses ||
		to == types.RelayFormatGemini
}
