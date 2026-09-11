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
	ConversionFeatureReasoning            OrdinaryConversionFeature = "reasoning"
	ConversionFeatureSessionState         OrdinaryConversionFeature = "session_state"
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
	var losses []OrdinaryConversionLoss
	var rejections []OrdinaryConversionLoss

	if supportsStructuredOutput(spec.From, spec.To) {
		supported = append(supported, ConversionFeatureStructuredOutput)
	} else {
		losses = append(losses, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeStructuredOutputUnsupported,
			Feature:     ConversionFeatureStructuredOutput,
			Description: "the target conversion does not preserve the source structured-output dialect",
		})
	}
	if supportsParallelToolControl(spec.From, spec.To) {
		supported = append(supported, ConversionFeatureParallelToolCalls)
	} else if spec.From != types.RelayFormatGemini && spec.To == types.RelayFormatGemini {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeUnsupportedParallelToolControl,
			Feature:     ConversionFeatureParallelToolCalls,
			Description: "Gemini generateContent does not expose an equivalent parallel-tool-call control",
		})
	}
	if spec.From != types.RelayFormatClaude && spec.To == types.RelayFormatClaude {
		losses = append(losses,
			OrdinaryConversionLoss{
				Code:        types.ConversionDiagnosticCodeClaudeSamplingRemoved,
				Feature:     ConversionFeatureOptionalScalars,
				Description: "Claude models with strict thinking controls remove temperature, top_p, and top_k",
			},
			OrdinaryConversionLoss{
				Code:        types.ConversionDiagnosticCodeClaudeSamplingConstrained,
				Feature:     ConversionFeatureOptionalScalars,
				Description: "Claude manual thinking removes temperature and top_k and constrains top_p",
			},
		)
	}

	if spec.From != types.RelayFormatGemini && spec.To == types.RelayFormatGemini {
		losses = append(losses, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeUnsupportedFunctionStrict,
			Feature:     ConversionFeatureFunctionTools,
			Description: "Gemini generateContent does not expose function strictness",
		})
	}
	if spec.From == types.RelayFormatOpenAIResponses && spec.To == types.RelayFormatGemini {
		losses = append(losses, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeCustomToolOmitted,
			Feature:     ConversionFeatureFunctionTools,
			Description: "Gemini generateContent omits OpenAI Responses custom and unknown tool definitions",
		})
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeUnsupportedOpaqueTool,
			Feature:     ConversionFeatureFunctionTools,
			Description: "Gemini generateContent cannot represent opaque OpenAI Responses tool definitions",
		})
	}
	rejections = append(rejections, OrdinaryConversionLoss{
		Code:        types.ConversionDiagnosticCodeUnsupportedHostedTool,
		Feature:     ConversionFeatureFunctionTools,
		Description: "the target protocol may not have a verified mapping for a source hosted tool",
	})
	if spec.To == types.RelayFormatGemini {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeUnverifiedToolMapping,
			Feature:     ConversionFeatureFunctionTools,
			Description: "code execution or URL context semantics do not have a verified Gemini mapping",
		})
	}
	if spec.From == types.RelayFormatClaude {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeVendorSpecificToolUnsupported,
			Feature:     ConversionFeatureFunctionTools,
			Description: "Claude MCP server configuration requires a verified target-protocol mapping",
		})
	}
	if spec.From == types.RelayFormatOpenAIResponses || spec.From == types.RelayFormatClaude || spec.From == types.RelayFormatGemini {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
			Feature:     ConversionFeatureReasoning,
			Description: "encrypted or signed provider reasoning state is not portable across protocols",
		})
	}
	if spec.From == types.RelayFormatOpenAIResponses || spec.From == types.RelayFormatClaude || spec.From == types.RelayFormatGemini {
		rejections = append(rejections, OrdinaryConversionLoss{
			Code:        types.ConversionDiagnosticCodeSessionReferenceUnsupported,
			Feature:     ConversionFeatureSessionState,
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
	switch from {
	case types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses:
		return to != types.RelayFormatClaude
	default:
		return false
	}
}

func supportsParallelToolControl(from types.RelayFormat, to types.RelayFormat) bool {
	return from != types.RelayFormatGemini && to != types.RelayFormatGemini
}
