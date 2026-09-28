package helper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

func ResolveIncomingBillingExprRequestInput(c *gin.Context, info *relaycommon.RelayInfo) (billingexpr.RequestInput, error) {
	if info != nil && info.BillingRequestInput != nil {
		input := cloneRequestInput(*info.BillingRequestInput)
		merged := cloneStringMap(info.RequestHeaders)
		maps.Copy(merged, input.Headers)
		input.Headers = merged
		return input, nil
	}

	input := billingexpr.RequestInput{}
	if info != nil {
		input.Headers = cloneStringMap(info.RequestHeaders)
	}

	bodyBytes, err := readIncomingBillingExprBody(c)
	if err != nil {
		return billingexpr.RequestInput{}, err
	}
	input.Body = bodyBytes
	if info != nil {
		return ResolveImageBillingRequestInput(c, info, input)
	}
	return input, nil
}

const seedreamLowerTierMaxPixels = 2_610_000

var seedreamImageSizePattern = regexp.MustCompile(`^([0-9]+)\s*[xX*]\s*([0-9]+)$`)

type seedreamRequestProfile struct {
	maxOutputs         int
	maxReferenceImages int
	layerDecomposition bool
}

// OutboundImageBilling contains only the final price-bearing image fields.
// Request keeps the client model as billing identity; Input is safe to retain
// because prompts, image bytes, and image URLs are excluded.
type OutboundImageBilling struct {
	Request     *dto.ImageRequest
	PromptTexts []string
	Input       billingexpr.RequestInput
	Count       int
}

func seedreamProfile(model string) (seedreamRequestProfile, bool) {
	switch strings.TrimSpace(model) {
	case "doubao-seedream-5-0-pro", "doubao-seedream-5-0-pro-260628":
		return seedreamRequestProfile{maxOutputs: 15, maxReferenceImages: 10, layerDecomposition: true}, true
	case "doubao-seedream-5-0", "doubao-seedream-5-0-260128",
		"doubao-seedream-4-5", "doubao-seedream-4-5-251128",
		"doubao-seedream-4-0", "doubao-seedream-4-0-20260415",
		"doubao-seedream-4-0-250828":
		return seedreamRequestProfile{maxOutputs: 15, maxReferenceImages: 14}, true
	default:
		return seedreamRequestProfile{}, false
	}
}

func seedreamReferenceImageCount(raw []byte) (int, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, nil
	}
	var single string
	if err := common.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return 0, fmt.Errorf("image must not be empty")
		}
		return 1, nil
	}
	var multiple []string
	if err := common.Unmarshal(raw, &multiple); err != nil {
		return 0, fmt.Errorf("image must be a string or string array")
	}
	for _, image := range multiple {
		if strings.TrimSpace(image) == "" {
			return 0, fmt.Errorf("image entries must not be empty")
		}
	}
	return len(multiple), nil
}

func seedreamSizeIsLowerTier(size string) bool {
	switch strings.ToUpper(strings.TrimSpace(size)) {
	case "1K", "1.5K":
		return true
	case "", "2K", "3K", "4K", "AUTO":
		return false
	}
	match := seedreamImageSizePattern.FindStringSubmatch(strings.TrimSpace(size))
	if len(match) != 3 {
		return false
	}
	width, widthErr := strconv.ParseUint(match[1], 10, 32)
	height, heightErr := strconv.ParseUint(match[2], 10, 32)
	if widthErr != nil || heightErr != nil || width == 0 || height == 0 {
		return false
	}
	return width <= seedreamLowerTierMaxPixels/height
}

// ResolveImageBillingRequestInput freezes only the validated scalar image
// parameters needed by pricing. Image files, prompts and base64 payloads are
// deliberately excluded, including for multipart edits.
func ResolveImageBillingRequestInput(c *gin.Context, info *relaycommon.RelayInfo, input billingexpr.RequestInput) (billingexpr.RequestInput, error) {
	if info == nil {
		return input, nil
	}
	request, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return input, nil
	}
	count, err := request.ImageCount(false)
	if err != nil {
		return input, err
	}
	profile, seedream := seedreamProfile(request.Model)
	if seedream {
		if count > profile.maxOutputs {
			return input, fmt.Errorf("n must be an integer between 1 and %d", profile.maxOutputs)
		}
		referenceImages, referenceErr := seedreamReferenceImageCount(request.Image)
		if referenceErr != nil {
			return input, referenceErr
		}
		if referenceImages > profile.maxReferenceImages {
			return input, fmt.Errorf("at most %d reference images are supported", profile.maxReferenceImages)
		}
		layered := request.LayerDecomposition != nil && *request.LayerDecomposition
		if layered {
			if !profile.layerDecomposition {
				return input, fmt.Errorf("layer_decomposition is not supported by this model")
			}
			if referenceImages != 1 {
				return input, fmt.Errorf("layer decomposition requires exactly one input image")
			}
			count = billingexpr.MaxRequestImageOutputs
		}
		lower, higher := float64(0), float64(count)
		if seedreamSizeIsLowerTier(request.Size) {
			lower, higher = float64(count), 0
		}
		references := float64(referenceImages)
		input.Body = nil
		input.ImageCount = &count
		input.ImagesUpTo1_5K = &lower
		input.ImagesAbove1_5K = &higher
		input.InputImages = &references
		return input, nil
	}
	if input.ImageCount != nil && len(input.Body) > 0 {
		input.ImageCount = &count
		return input, nil
	}
	body := map[string]any{"model": request.Model, "n": count, "size": request.Size, "quality": request.Quality}
	if request.BillingParameters != nil {
		body["parameters"] = request.BillingParameters
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return input, err
	}
	input.Body = encoded
	input.ImageCount = &count
	return input, nil
}

// ResolveOutboundImageBillingJSON rebuilds the billing view from the exact
// JSON sent upstream while retaining the client model as billing identity.
func ResolveOutboundImageBillingJSON(info *relaycommon.RelayInfo, outboundJSON []byte, schemaConfirmed ...bool) (*OutboundImageBilling, error) {
	if info == nil {
		return nil, nil
	}
	incoming, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return nil, nil
	}
	if info.ChannelMeta == nil {
		return nil, fmt.Errorf("image billing API type is required")
	}

	switch info.ApiType {
	case constant.APITypeGemini, constant.APITypeVertexAi:
		return resolveGeminiImageBilling(info, incoming, outboundJSON)
	case constant.APITypeReplicate:
		return resolveReplicateImageBilling(info, incoming, outboundJSON)
	case constant.APITypeSiliconFlow:
		return resolveSiliconFlowImageBilling(info, incoming, outboundJSON)
	case constant.APITypeMiniMax:
		return resolveMiniMaxImageBilling(info, incoming, outboundJSON)
	case constant.APITypeJimeng:
		return resolveJimengImageBilling(info, incoming, outboundJSON)
	case constant.APITypeXai:
		return resolveXAIImageBilling(info, incoming, outboundJSON)
	default:
		confirmed := len(schemaConfirmed) > 0 && schemaConfirmed[0]
		if isOpenAIImageBillingAPIType(info.ApiType, confirmed) {
			return resolveOpenAIImageBilling(info, incoming, outboundJSON)
		}
		return nil, fmt.Errorf("unsupported image billing schema for API type %d", info.ApiType)
	}
}

func isOpenAIImageBillingAPIType(apiType int, schemaConfirmed bool) bool {
	switch apiType {
	case constant.APITypeOpenAI,
		constant.APITypeZhipuV4,
		constant.APITypeVolcEngine,
		constant.APITypeOpenRouter,
		constant.APITypeXinference,
		constant.APITypeMoonshot,
		constant.APITypeSub2API,
		constant.APITypeNewAPI:
		return true
	case constant.APITypeAdvancedCustom:
		return schemaConfirmed
	default:
		return false
	}
}

func resolveOpenAIImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "OpenAI image request")
	if err != nil {
		return nil, err
	}
	prompt, err := optionalImageBillingString(object, "prompt", "prompt")
	if err != nil {
		return nil, err
	}
	count, err := imageBillingCount(object, "n", "n", true)
	if err != nil {
		return nil, err
	}
	body := canonicalImageBillingBody(incoming.Model, count)
	err = copyImageBillingScalars(body, object,
		"size", "quality", "response_format", "style", "background", "moderation",
		"output_format", "output_compression", "partial_images", "stream",
		"input_fidelity", "watermark", "layer_decomposition", "watermark_enabled",
	)
	if err != nil {
		return nil, err
	}
	copyCanonicalStringAlias(body, "size", "resolution")

	var parameters *dto.ImageBillingParameters
	if raw, exists := object["parameters"]; exists && !isJSONNull(raw) {
		parameterObject, objectErr := imageBillingObject(raw, "parameters")
		if objectErr != nil {
			return nil, objectErr
		}
		safeParameters := map[string]any{}
		if copyErr := copyImageBillingScalars(safeParameters, parameterObject, "n", "prompt_extend"); copyErr != nil {
			return nil, copyErr
		}
		parameters = &dto.ImageBillingParameters{}
		if unmarshalErr := common.Unmarshal(raw, parameters); unmarshalErr != nil {
			return nil, fmt.Errorf("invalid image parameters: %w", unmarshalErr)
		}
		if parameters.N != nil && *parameters.N > dto.MaxImageN {
			return nil, fmt.Errorf("parameters.n must be an integer between 0 and %d", dto.MaxImageN)
		}
		if len(safeParameters) > 0 {
			body["parameters"] = safeParameters
		}
	}

	size, err := optionalImageBillingString(object, "size", "size")
	if err != nil {
		return nil, err
	}
	quality, err := optionalImageBillingString(object, "quality", "quality")
	if err != nil {
		return nil, err
	}
	layerDecomposition, err := optionalImageBillingBool(object, "layer_decomposition", "layer_decomposition")
	if err != nil {
		return nil, err
	}
	referenceImages := 0
	if _, seedream := seedreamProfile(incoming.Model); seedream {
		referenceImages, err = seedreamReferenceImageCount(object["image"])
		if err != nil {
			return nil, err
		}
	}
	var promptTexts []string
	if prompt != "" {
		promptTexts = append(promptTexts, prompt)
	}
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model:              incoming.Model,
		N:                  common.GetPointer(uint(count)),
		Size:               size,
		Quality:            quality,
		LayerDecomposition: layerDecomposition,
		BillingParameters:  parameters,
	}, referenceImages, body, promptTexts)
}

func resolveGeminiImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "Gemini image request")
	if err != nil {
		return nil, err
	}
	var instances []json.RawMessage
	if raw, exists := object["instances"]; !exists || isJSONNull(raw) {
		return nil, fmt.Errorf("instances[0].prompt is required")
	} else if err = common.Unmarshal(raw, &instances); err != nil || len(instances) == 0 {
		return nil, fmt.Errorf("instances[0].prompt is required")
	}
	instance, err := imageBillingObject(instances[0], "instances[0]")
	if err != nil {
		return nil, err
	}
	prompt, err := requiredImageBillingString(instance, "prompt", "instances[0].prompt")
	if err != nil {
		return nil, err
	}
	parametersRaw, exists := object["parameters"]
	if !exists || isJSONNull(parametersRaw) {
		return nil, fmt.Errorf("parameters.sampleCount is required")
	}
	parameters, err := imageBillingObject(parametersRaw, "parameters")
	if err != nil {
		return nil, err
	}
	count, err := imageBillingCount(parameters, "sampleCount", "parameters.sampleCount", false)
	if err != nil {
		return nil, err
	}
	safeParameters := map[string]any{}
	if err = copyImageBillingScalars(safeParameters, parameters, "sampleCount", "aspectRatio", "imageSize", "personGeneration"); err != nil {
		return nil, err
	}
	body := canonicalImageBillingBody(incoming.Model, count)
	body["parameters"] = safeParameters
	aspectRatio, err := optionalImageBillingString(parameters, "aspectRatio", "parameters.aspectRatio")
	if err != nil {
		return nil, err
	}
	imageSize, err := optionalImageBillingString(parameters, "imageSize", "parameters.imageSize")
	if err != nil {
		return nil, err
	}
	setCanonicalString(body, "size", aspectRatio)
	setCanonicalString(body, "resolution", imageSize)
	setCanonicalString(body, "quality", imageSize)
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model: incoming.Model, N: common.GetPointer(uint(count)), Size: aspectRatio, Quality: imageSize,
	}, 0, body, []string{prompt})
}

func resolveReplicateImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "Replicate image request")
	if err != nil {
		return nil, err
	}
	inputRaw, exists := object["input"]
	if !exists || isJSONNull(inputRaw) {
		return nil, fmt.Errorf("input.prompt is required")
	}
	input, err := imageBillingObject(inputRaw, "input")
	if err != nil {
		return nil, err
	}
	prompt, err := requiredImageBillingString(input, "prompt", "input.prompt")
	if err != nil {
		return nil, err
	}
	count, err := imageBillingCount(input, "num_outputs", "input.num_outputs", false)
	if err != nil {
		return nil, err
	}
	safeInput := map[string]any{}
	if err = copyImageBillingScalars(safeInput, input,
		"num_outputs", "aspect_ratio", "width", "height", "prompt_upsampling", "output_format",
	); err != nil {
		return nil, err
	}
	body := canonicalImageBillingBody(incoming.Model, count)
	body["input"] = safeInput
	aspectRatio, err := optionalImageBillingString(input, "aspect_ratio", "input.aspect_ratio")
	if err != nil {
		return nil, err
	}
	size := aspectRatio
	resolution := imageBillingDimensions(input)
	if size == "" {
		size = resolution
	}
	setCanonicalString(body, "size", size)
	setCanonicalString(body, "resolution", resolution)
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model: incoming.Model, N: common.GetPointer(uint(count)), Size: size,
	}, 0, body, []string{prompt})
}

func resolveSiliconFlowImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "SiliconFlow image request")
	if err != nil {
		return nil, err
	}
	prompt, err := requiredImageBillingString(object, "prompt", "prompt")
	if err != nil {
		return nil, err
	}
	promptTexts := []string{prompt}
	if negativePrompt, promptErr := optionalImageBillingString(object, "negative_prompt", "negative_prompt"); promptErr != nil {
		return nil, promptErr
	} else if negativePrompt != "" {
		promptTexts = append(promptTexts, negativePrompt)
	}
	count, err := imageBillingCount(object, "batch_size", "batch_size", false)
	if err != nil {
		return nil, err
	}
	body := canonicalImageBillingBody(incoming.Model, count)
	if err = copyImageBillingScalars(body, object,
		"batch_size", "image_size", "seed", "num_inference_steps", "guidance_scale", "cfg",
	); err != nil {
		return nil, err
	}
	if _, exists := body["batch_size"]; !exists {
		body["batch_size"] = count
	}
	imageSize, err := optionalImageBillingString(object, "image_size", "image_size")
	if err != nil {
		return nil, err
	}
	setCanonicalString(body, "size", imageSize)
	setCanonicalString(body, "resolution", imageSize)
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model: incoming.Model, N: common.GetPointer(uint(count)), Size: imageSize,
	}, 0, body, promptTexts)
}

func resolveMiniMaxImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "MiniMax image request")
	if err != nil {
		return nil, err
	}
	prompt, err := requiredImageBillingString(object, "prompt", "prompt")
	if err != nil {
		return nil, err
	}
	count, err := imageBillingCount(object, "n", "n", true)
	if err != nil {
		return nil, err
	}
	body := canonicalImageBillingBody(incoming.Model, count)
	if err = copyImageBillingScalars(body, object,
		"n", "aspect_ratio", "response_format", "prompt_optimizer", "aigc_watermark",
	); err != nil {
		return nil, err
	}
	body["n"] = count
	aspectRatio, err := optionalImageBillingString(object, "aspect_ratio", "aspect_ratio")
	if err != nil {
		return nil, err
	}
	setCanonicalString(body, "size", aspectRatio)
	setCanonicalString(body, "resolution", aspectRatio)
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model: incoming.Model, N: common.GetPointer(uint(count)), Size: aspectRatio,
	}, 0, body, []string{prompt})
}

func resolveJimengImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "Jimeng image request")
	if err != nil {
		return nil, err
	}
	prompt, err := requiredImageBillingString(object, "prompt", "prompt")
	if err != nil {
		return nil, err
	}
	const count = 1
	body := canonicalImageBillingBody(incoming.Model, count)
	if err = copyImageBillingNumbers(body, object, "", "width", "height"); err != nil {
		return nil, err
	}
	if err = copyImageBillingBooleans(body, object, "", "use_pre_llm", "use_sr", "return_url"); err != nil {
		return nil, err
	}
	if raw, exists := object["logo_info"]; exists && !isJSONNull(raw) {
		logo, objectErr := imageBillingObject(raw, "logo_info")
		if objectErr != nil {
			return nil, objectErr
		}
		safeLogo := map[string]any{}
		if copyErr := copyImageBillingBooleans(safeLogo, logo, "logo_info.", "add_logo"); copyErr != nil {
			return nil, copyErr
		}
		if copyErr := copyImageBillingNumbers(safeLogo, logo, "logo_info.", "position", "language", "opacity"); copyErr != nil {
			return nil, copyErr
		}
		if len(safeLogo) > 0 {
			body["logo_info"] = safeLogo
		}
	}
	resolution := imageBillingDimensions(object)
	setCanonicalString(body, "size", resolution)
	setCanonicalString(body, "resolution", resolution)
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model: incoming.Model, N: common.GetPointer(uint(count)), Size: resolution,
	}, 0, body, []string{prompt})
}

func resolveXAIImageBilling(info *relaycommon.RelayInfo, incoming *dto.ImageRequest, outboundJSON []byte) (*OutboundImageBilling, error) {
	object, err := imageBillingObject(outboundJSON, "xAI image request")
	if err != nil {
		return nil, err
	}
	prompt, err := requiredImageBillingString(object, "prompt", "prompt")
	if err != nil {
		return nil, err
	}
	count, err := imageBillingCount(object, "n", "n", true)
	if err != nil {
		return nil, err
	}
	body := canonicalImageBillingBody(incoming.Model, count)
	if err = copyImageBillingScalars(body, object, "n", "response_format"); err != nil {
		return nil, err
	}
	body["n"] = count
	return resolveOutboundImageBilling(info, &dto.ImageRequest{
		Model: incoming.Model, N: common.GetPointer(uint(count)),
	}, 0, body, []string{prompt})
}

func imageBillingObject(raw []byte, name string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := common.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", name, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	return object, nil
}

func requiredImageBillingString(object map[string]json.RawMessage, key, path string) (string, error) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return "", fmt.Errorf("%s is required", path)
	}
	var value string
	if err := common.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", path)
	}
	return value, nil
}

func optionalImageBillingString(object map[string]json.RawMessage, key, path string) (string, error) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return "", nil
	}
	var value string
	if err := common.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", path)
	}
	return value, nil
}

func optionalImageBillingBool(object map[string]json.RawMessage, key, path string) (*bool, error) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return nil, nil
	}
	var value bool
	if err := common.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s must be a boolean", path)
	}
	return common.GetPointer(value), nil
}

func imageBillingCount(object map[string]json.RawMessage, key, path string, defaultOne bool) (int, error) {
	raw, exists := object[key]
	if !exists {
		if defaultOne {
			return 1, nil
		}
		return 0, fmt.Errorf("%s is required", path)
	}
	if isJSONNull(raw) {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", path, dto.MaxImageN)
	}
	var number json.Number
	if err := common.Unmarshal(raw, &number); err != nil {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", path, dto.MaxImageN)
	}
	value, err := strconv.ParseUint(number.String(), 10, 64)
	if err != nil || value < 1 || value > dto.MaxImageN {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", path, dto.MaxImageN)
	}
	return int(value), nil
}

func copyImageBillingScalars(destination map[string]any, source map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		raw, exists := source[key]
		if !exists || isJSONNull(raw) {
			continue
		}
		value, err := imageBillingScalar(raw)
		if err != nil {
			return fmt.Errorf("%s must be a string, number, or boolean", key)
		}
		destination[key] = value
	}
	return nil
}

func copyImageBillingNumbers(destination map[string]any, source map[string]json.RawMessage, prefix string, keys ...string) error {
	for _, key := range keys {
		raw, exists := source[key]
		if !exists || isJSONNull(raw) {
			continue
		}
		value, err := imageBillingScalar(raw)
		if err != nil {
			return fmt.Errorf("%s%s must be a number", prefix, key)
		}
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s%s must be a number", prefix, key)
		}
		destination[key] = value
	}
	return nil
}

func copyImageBillingBooleans(destination map[string]any, source map[string]json.RawMessage, prefix string, keys ...string) error {
	for _, key := range keys {
		raw, exists := source[key]
		if !exists || isJSONNull(raw) {
			continue
		}
		value, err := imageBillingScalar(raw)
		if err != nil {
			return fmt.Errorf("%s%s must be a boolean", prefix, key)
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s%s must be a boolean", prefix, key)
		}
		destination[key] = value
	}
	return nil
}

func imageBillingScalar(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	switch value.(type) {
	case string, bool, json.Number:
		return value, nil
	default:
		return nil, fmt.Errorf("not a scalar")
	}
}

func canonicalImageBillingBody(model string, count int) map[string]any {
	return map[string]any{"model": model, "n": count}
}

func copyCanonicalStringAlias(body map[string]any, sourceKey, destinationKey string) {
	if value, ok := body[sourceKey].(string); ok && value != "" {
		body[destinationKey] = value
	}
}

func setCanonicalString(body map[string]any, key, value string) {
	if value != "" {
		body[key] = value
	}
}

func imageBillingDimensions(object map[string]json.RawMessage) string {
	width, widthOK := imageBillingPositiveInteger(object["width"])
	height, heightOK := imageBillingPositiveInteger(object["height"])
	if !widthOK || !heightOK {
		return ""
	}
	return fmt.Sprintf("%dx%d", width, height)
}

func imageBillingPositiveInteger(raw json.RawMessage) (uint64, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return 0, false
	}
	var number json.Number
	if err := common.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	value, err := strconv.ParseUint(number.String(), 10, 64)
	return value, err == nil && value > 0
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// ResolveOutboundImageBillingMultipart rebuilds the billing view from the
// final multipart body without retaining prompts or file contents.
func ResolveOutboundImageBillingMultipart(info *relaycommon.RelayInfo, contentType string, body io.Reader, schemaConfirmed ...bool) (*OutboundImageBilling, error) {
	if info == nil {
		return nil, nil
	}
	incoming, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return nil, nil
	}
	confirmed := len(schemaConfirmed) > 0 && schemaConfirmed[0]
	if info.ChannelMeta == nil || !isOpenAIImageBillingAPIType(info.ApiType, confirmed) {
		return nil, fmt.Errorf("unsupported multipart image billing schema")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") || params["boundary"] == "" {
		return nil, fmt.Errorf("invalid multipart content type")
	}

	outbound := &dto.ImageRequest{Model: incoming.Model}
	referenceImages := 0
	seen := map[string]bool{}
	reader := multipart.NewReader(body, params["boundary"])
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		name := part.FormName()
		if name == "image" || name == "image[]" {
			referenceImages++
			_, err = io.Copy(io.Discard, part)
			_ = part.Close()
			if err != nil {
				return nil, err
			}
			continue
		}
		switch name {
		case "prompt", "n", "size", "quality", "layer_decomposition", "parameters":
			if seen[name] {
				_, err = io.Copy(io.Discard, part)
				_ = part.Close()
				if err != nil {
					return nil, err
				}
				continue
			}
			seen[name] = true
			var value string
			var valueErr error
			if name == "prompt" {
				var prompt []byte
				prompt, valueErr = io.ReadAll(part)
				value = string(prompt)
			} else {
				value, valueErr = readMultipartBillingScalar(part)
			}
			_ = part.Close()
			if valueErr != nil {
				return nil, valueErr
			}
			switch name {
			case "prompt":
				outbound.Prompt = value
			case "n":
				parsed, parseErr := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
				if parseErr != nil || parsed > dto.MaxImageN {
					return nil, fmt.Errorf("n must be an integer between 1 and %d", dto.MaxImageN)
				}
				outbound.N = common.GetPointer(uint(parsed))
			case "size":
				outbound.Size = value
			case "quality":
				outbound.Quality = value
			case "layer_decomposition":
				parsed, parseErr := strconv.ParseBool(strings.TrimSpace(value))
				if parseErr != nil {
					return nil, fmt.Errorf("invalid layer_decomposition: %w", parseErr)
				}
				outbound.LayerDecomposition = common.GetPointer(parsed)
			case "parameters":
				parameters := &dto.ImageBillingParameters{}
				if parseErr := common.Unmarshal([]byte(value), parameters); parseErr != nil {
					return nil, fmt.Errorf("invalid image parameters: %w", parseErr)
				}
				outbound.BillingParameters = parameters
			}
		default:
			_, err = io.Copy(io.Discard, part)
			_ = part.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	promptTexts := []string{outbound.Prompt}
	outbound.Prompt = ""
	return resolveOutboundImageBilling(info, outbound, referenceImages, nil, promptTexts)
}

func resolveOutboundImageBilling(
	info *relaycommon.RelayInfo,
	outbound *dto.ImageRequest,
	referenceImages int,
	sanitizedBody map[string]any,
	promptTexts []string,
) (*OutboundImageBilling, error) {
	incoming, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return nil, nil
	}
	profile, seedream := seedreamProfile(incoming.Model)
	if outbound.N != nil && *outbound.N == 0 {
		maxOutputs := dto.MaxImageN
		if seedream {
			maxOutputs = profile.maxOutputs
		}
		return nil, fmt.Errorf("n must be an integer between 1 and %d", maxOutputs)
	}

	billingRequest := &dto.ImageRequest{
		Model:              incoming.Model,
		N:                  outbound.N,
		Size:               outbound.Size,
		Quality:            outbound.Quality,
		LayerDecomposition: outbound.LayerDecomposition,
		BillingParameters:  outbound.BillingParameters,
	}
	if seedream && referenceImages > 0 {
		references := make([]string, referenceImages)
		for index := range references {
			references[index] = "billing-reference"
		}
		encoded, err := common.Marshal(references)
		if err != nil {
			return nil, err
		}
		billingRequest.Image = encoded
	}
	count, err := billingRequest.ImageCount(false)
	if err != nil {
		return nil, err
	}

	input := billingexpr.RequestInput{Headers: cloneStringMap(info.RequestHeaders)}
	if info.BillingRequestInput != nil {
		input.Headers = cloneStringMap(info.BillingRequestInput.Headers)
		input.EvaluatedAtUnix = info.BillingRequestInput.EvaluatedAtUnix
	}
	var resolved billingexpr.RequestInput
	if seedream {
		resolved, err = ResolveImageBillingRequestInput(nil, &relaycommon.RelayInfo{Request: billingRequest}, input)
		if err != nil {
			return nil, err
		}
	} else {
		if sanitizedBody == nil {
			sanitizedBody = canonicalImageBillingBody(incoming.Model, count)
			setCanonicalString(sanitizedBody, "size", billingRequest.Size)
			setCanonicalString(sanitizedBody, "resolution", billingRequest.Size)
			setCanonicalString(sanitizedBody, "quality", billingRequest.Quality)
			if parameters := billingRequest.BillingParameters; parameters != nil {
				safeParameters := map[string]any{}
				if parameters.N != nil {
					safeParameters["n"] = *parameters.N
				}
				if parameters.PromptExtend != nil {
					safeParameters["prompt_extend"] = *parameters.PromptExtend
				}
				if len(safeParameters) > 0 {
					sanitizedBody["parameters"] = safeParameters
				}
			}
		}
		sanitizedBody["model"] = incoming.Model
		sanitizedBody["n"] = count
		encoded, marshalErr := common.Marshal(sanitizedBody)
		if marshalErr != nil {
			return nil, marshalErr
		}
		input.Body = encoded
		input.ImageCount = common.GetPointer(count)
		resolved = input
	}
	return &OutboundImageBilling{
		Request:     billingRequest,
		PromptTexts: append([]string(nil), promptTexts...),
		Input:       resolved,
		Count:       count,
	}, nil
}

func readMultipartBillingScalar(part io.Reader) (string, error) {
	const maxBillingScalarBytes = 64 << 10
	value, err := io.ReadAll(io.LimitReader(part, maxBillingScalarBytes+1))
	if err != nil {
		return "", err
	}
	if len(value) > maxBillingScalarBytes {
		return "", fmt.Errorf("multipart billing parameter exceeds %d bytes", maxBillingScalarBytes)
	}
	return string(value), nil
}

// ResolveOutboundSeedreamBillingRequestInput is retained for callers that only
// need the Seedream scalar snapshot.
func ResolveOutboundSeedreamBillingRequestInput(info *relaycommon.RelayInfo, outboundJSON []byte) (*billingexpr.RequestInput, error) {
	if info == nil {
		return nil, nil
	}
	incoming, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return nil, nil
	}
	if _, seedream := seedreamProfile(incoming.Model); !seedream {
		return nil, nil
	}
	resolved, err := ResolveOutboundImageBillingJSON(info, outboundJSON)
	if err != nil || resolved == nil {
		return nil, err
	}
	return &resolved.Input, nil
}

func BuildBillingExprRequestInputFromRequest(request dto.Request, headers map[string]string) (billingexpr.RequestInput, error) {
	input := billingexpr.RequestInput{
		Headers: cloneStringMap(headers),
	}
	if request == nil {
		return input, nil
	}
	if imageRequest, ok := request.(*dto.ImageRequest); ok {
		return ResolveImageBillingRequestInput(nil, &relaycommon.RelayInfo{Request: imageRequest}, input)
	}

	bodyBytes, err := common.Marshal(request)
	if err != nil {
		return billingexpr.RequestInput{}, err
	}
	input.Body = bodyBytes
	return input, nil
}

func readIncomingBillingExprBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || !isJSONContentType(c.Request.Header.Get("Content-Type")) {
		return nil, nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	return storage.Bytes()
}

func cloneRequestInput(src billingexpr.RequestInput) billingexpr.RequestInput {
	input := billingexpr.RequestInput{
		Headers:         cloneStringMap(src.Headers),
		EvaluatedAtUnix: src.EvaluatedAtUnix,
	}
	if src.ImageCount != nil {
		count := *src.ImageCount
		input.ImageCount = &count
	}
	if src.ImagesUpTo1_5K != nil {
		count := *src.ImagesUpTo1_5K
		input.ImagesUpTo1_5K = &count
	}
	if src.ImagesAbove1_5K != nil {
		count := *src.ImagesAbove1_5K
		input.ImagesAbove1_5K = &count
	}
	if src.InputImages != nil {
		count := *src.InputImages
		input.InputImages = &count
	}
	if len(src.Body) > 0 {
		input.Body = append([]byte(nil), src.Body...)
	}
	if len(src.Usage) > 0 {
		input.Usage = make(map[string]any, len(src.Usage))
		maps.Copy(input.Usage, src.Usage)
	}
	return input
}

func isJSONContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(contentType, "application/json")
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		if strings.TrimSpace(key) == "" {
			continue
		}
		dst[key] = value
	}
	return dst
}
