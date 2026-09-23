package toolconv

import (
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	oairesponses "github.com/QuantumNous/new-api/relaykit/relayconvert/internal/oai_responses"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
)

func InspectRequest(from types.RelayFormat, to types.RelayFormat, request any) []types.ConversionDiagnostic {
	if from == to {
		return nil
	}

	var diagnostics []types.ConversionDiagnostic
	switch value := request.(type) {
	case *dto.GeneralOpenAIRequest:
		diagnostics = inspectOpenAIChatRequest(value, to)
	case dto.GeneralOpenAIRequest:
		diagnostics = inspectOpenAIChatRequest(&value, to)
	case *dto.OpenAIResponsesRequest:
		diagnostics = inspectOpenAIResponsesRequest(value, to)
	case dto.OpenAIResponsesRequest:
		diagnostics = inspectOpenAIResponsesRequest(&value, to)
	case *dto.ClaudeRequest:
		diagnostics = inspectClaudeRequest(value)
	case dto.ClaudeRequest:
		diagnostics = inspectClaudeRequest(&value)
	case *dto.GeminiChatRequest:
		diagnostics = inspectGeminiRequest(value)
	case dto.GeminiChatRequest:
		diagnostics = inspectGeminiRequest(&value)
	}
	for i := range diagnostics {
		diagnostics[i].From = from
		diagnostics[i].To = to
	}
	return diagnostics
}

func inspectOpenAIChatRequest(request *dto.GeneralOpenAIRequest, to types.RelayFormat) []types.ConversionDiagnostic {
	if request == nil || request.ResponseFormat == nil || to != types.RelayFormatClaude {
		return nil
	}
	return []types.ConversionDiagnostic{requestPresentationLoss(
		"response_format",
		types.ConversionDiagnosticCodeStructuredOutputUnsupported,
		"Claude conversion does not preserve the OpenAI Chat structured-output configuration",
	)}
}

func inspectOpenAIResponsesRequest(request *dto.OpenAIResponsesRequest, to types.RelayFormat) []types.ConversionDiagnostic {
	if request == nil {
		return nil
	}
	diagnostics := oairesponses.UnsupportedStatefulFieldDiagnostics(request, to)
	if rawJSONPresent(request.Text) && to == types.RelayFormatClaude {
		diagnostics = append(diagnostics, requestPresentationLoss(
			"text.format",
			types.ConversionDiagnosticCodeStructuredOutputUnsupported,
			"Claude conversion does not preserve the OpenAI Responses structured-output configuration",
		))
	}
	if kitutil.GetJsonType(request.Input) == "array" {
		var items []map[string]any
		if err := kitutil.Unmarshal(request.Input, &items); err == nil {
			for index, item := range items {
				if item["type"] != "reasoning" || item["encrypted_content"] == nil {
					continue
				}
				diagnostics = append(diagnostics, requestSemanticLoss(
					fmt.Sprintf("input[%d].encrypted_content", index),
					types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
					"OpenAI Responses encrypted reasoning state is not portable across protocols",
				))
			}
		}
	}
	return diagnostics
}

func inspectClaudeRequest(request *dto.ClaudeRequest) []types.ConversionDiagnostic {
	if request == nil {
		return nil
	}

	var diagnostics []types.ConversionDiagnostic
	if rawJSONPresent(request.ContextManagement) {
		diagnostics = append(diagnostics, requestSemanticLoss(
			"context_management",
			types.ConversionDiagnosticCodeSessionReferenceUnsupported,
			"Claude context management state is not portable across protocols",
		))
	}
	if rawJSONPresent(request.Container) {
		diagnostics = append(diagnostics, requestSemanticLoss(
			"container",
			types.ConversionDiagnosticCodeSessionReferenceUnsupported,
			"Claude container state is not portable across protocols",
		))
	}
	if rawJSONPresent(request.McpServers) {
		diagnostics = append(diagnostics, requestSemanticLoss(
			"mcp_servers",
			types.ConversionDiagnosticCodeVendorSpecificToolUnsupported,
			"Claude MCP server configuration requires a verified target-protocol mapping",
		))
	}
	if rawJSONPresent(request.OutputFormat) {
		diagnostics = append(diagnostics, requestPresentationLoss(
			"output_format",
			types.ConversionDiagnosticCodeStructuredOutputUnsupported,
			"Claude structured-output configuration is not portable across protocols",
		))
	}
	for messageIndex := range request.Messages {
		blocks, err := request.Messages[messageIndex].ParseContent()
		if err != nil {
			continue
		}
		for blockIndex := range blocks {
			block := blocks[blockIndex]
			if block.Type != "redacted_thinking" && block.Signature == "" {
				continue
			}
			diagnostics = append(diagnostics, requestSemanticLoss(
				fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex),
				types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
				"Claude encrypted or signed reasoning state is not portable across protocols",
			))
		}
	}
	return diagnostics
}

func inspectGeminiRequest(request *dto.GeminiChatRequest) []types.ConversionDiagnostic {
	if request == nil {
		return nil
	}
	var diagnostics []types.ConversionDiagnostic
	if request.CachedContent != "" {
		diagnostics = append(diagnostics, requestSemanticLoss(
			"cachedContent",
			types.ConversionDiagnosticCodeSessionReferenceUnsupported,
			"Gemini cached content references are not portable across protocols",
		))
	}
	if request.GenerationConfig.ResponseMimeType != "" || request.GenerationConfig.ResponseSchema != nil {
		diagnostics = append(diagnostics, requestPresentationLoss(
			"generationConfig.responseSchema",
			types.ConversionDiagnosticCodeStructuredOutputUnsupported,
			"Gemini structured-output configuration is not portable across protocols",
		))
	}
	for contentIndex := range request.Contents {
		for partIndex := range request.Contents[contentIndex].Parts {
			if len(request.Contents[contentIndex].Parts[partIndex].ThoughtSignature) == 0 {
				continue
			}
			diagnostics = append(diagnostics, requestSemanticLoss(
				fmt.Sprintf("contents[%d].parts[%d].thoughtSignature", contentIndex, partIndex),
				types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
				"Gemini thought signatures are not portable across protocols",
			))
		}
	}
	return diagnostics
}

func requestSemanticLoss(path string, code string, message string) types.ConversionDiagnostic {
	return types.ConversionDiagnostic{
		Code:     code,
		Path:     path,
		Message:  message,
		Severity: types.ConversionDiagnosticError,
	}
}

func requestPresentationLoss(path string, code string, message string) types.ConversionDiagnostic {
	return types.ConversionDiagnostic{
		Code:     code,
		Path:     path,
		Message:  message,
		Severity: types.ConversionDiagnosticWarning,
	}
}
