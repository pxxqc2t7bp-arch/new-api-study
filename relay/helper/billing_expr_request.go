package helper

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
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

// ResolveOutboundSeedreamBillingRequestInput rebuilds the Seedream pricing
// scalars from the exact JSON that will be sent upstream. It returns nil for
// other image models so their existing billing paths remain unchanged.
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

	var outbound struct {
		N                  *uint           `json:"n"`
		Size               string          `json:"size"`
		Image              json.RawMessage `json:"image"`
		LayerDecomposition *bool           `json:"layer_decomposition"`
	}
	if err := common.Unmarshal(outboundJSON, &outbound); err != nil {
		return nil, err
	}

	input := billingexpr.RequestInput{Headers: cloneStringMap(info.RequestHeaders)}
	if info.BillingRequestInput != nil {
		input.Headers = cloneStringMap(info.BillingRequestInput.Headers)
		input.EvaluatedAtUnix = info.BillingRequestInput.EvaluatedAtUnix
	}
	resolved, err := ResolveImageBillingRequestInput(nil, &relaycommon.RelayInfo{Request: &dto.ImageRequest{
		Model:              incoming.Model,
		N:                  outbound.N,
		Size:               outbound.Size,
		Image:              outbound.Image,
		LayerDecomposition: outbound.LayerDecomposition,
	}}, input)
	if err != nil {
		return nil, err
	}
	return &resolved, nil
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
