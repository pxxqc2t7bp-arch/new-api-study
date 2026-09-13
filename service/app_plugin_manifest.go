package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	AppManifestMaxBytes                = 64 * 1024
	AppManifestInvalidErrorCode        = "app_manifest_invalid"
	AppManifestForbiddenFieldErrorCode = "app_manifest_forbidden_field"
)

var (
	appManifestSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	strictSemverPattern    = regexp.MustCompile(
		`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
			`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)` +
			`(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?` +
			`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
	)
	appManifestJSONObjectSchemas = map[string]map[string]appManifestJSONFieldSchema{
		"root": {
			"apiVersion":      {},
			"kind":            {},
			"key":             {},
			"name":            {object: "name"},
			"version":         {},
			"callbackPath":    {},
			"surfaces":        {object: "surfaces"},
			"requestedScopes": {},
			"requires":        {object: "requires"},
		},
		"name": {
			"en": {},
			"zh": {},
		},
		"surfaces": {
			"direct":   {object: "surface"},
			"embedded": {object: "surface"},
		},
		"surface": {
			"startPath": {},
		},
		"requires": {
			"taskPlugins": {arrayObject: "taskPlugin"},
		},
		"taskPlugin": {
			"key":            {},
			"minimumVersion": {},
		},
	}
)

type appManifestJSONFieldSchema struct {
	object      string
	arrayObject string
}

// AppManifest is the validated, declarative App Plugin v1 manifest.
type AppManifest struct {
	APIVersion      int                     `json:"apiVersion"`
	Kind            string                  `json:"kind"`
	Key             string                  `json:"key"`
	Name            AppManifestName         `json:"name"`
	Version         string                  `json:"version"`
	CallbackPath    string                  `json:"callbackPath"`
	Surfaces        AppManifestSurfaces     `json:"surfaces"`
	RequestedScopes []string                `json:"requestedScopes"`
	Requires        AppManifestRequirements `json:"requires"`
}

type AppManifestName struct {
	EN string `json:"en"`
	ZH string `json:"zh"`
}

type AppManifestSurfaces struct {
	Direct   AppManifestSurface `json:"direct"`
	Embedded AppManifestSurface `json:"embedded"`
}

type AppManifestSurface struct {
	StartPath string `json:"startPath"`
}

type AppManifestRequirements struct {
	TaskPlugins []AppManifestTaskPluginRequirement `json:"taskPlugins"`
}

type AppManifestTaskPluginRequirement struct {
	Key            string `json:"key"`
	MinimumVersion string `json:"minimumVersion"`
}

// AppManifestError carries a stable code and only controlled, non-secret text.
type AppManifestError struct {
	Code        string
	FieldErrors []string
}

func (e *AppManifestError) Error() string {
	if len(e.FieldErrors) == 0 {
		return e.Code
	}
	return e.Code + ": " + strings.Join(e.FieldErrors, "; ")
}

// ValidateAppManifest validates and decodes one App Plugin manifest v1 object.
func ValidateAppManifest(raw []byte) (AppManifest, error) {
	if len(raw) > AppManifestMaxBytes {
		return AppManifest{}, invalidAppManifest("manifest exceeds size limit")
	}
	if !utf8.Valid(raw) {
		return AppManifest{}, invalidAppManifest("manifest must be valid UTF-8")
	}
	if err := scanAppManifestJSON(raw); err != nil {
		return AppManifest{}, err
	}
	if err := validateAppManifestJSONSchema(raw); err != nil {
		return AppManifest{}, err
	}

	var manifest AppManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return AppManifest{}, invalidAppManifest("manifest does not match the v1 schema")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return AppManifest{}, err
	}
	if !validAppManifest(manifest) {
		return AppManifest{}, invalidAppManifest("manifest contains an invalid value")
	}
	return manifest, nil
}

func scanAppManifestJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return invalidAppManifest("manifest must be valid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return invalidAppManifest("manifest must be one JSON object")
	}
	forbidden, err := scanAppManifestObject(decoder)
	if err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	if forbidden {
		return forbiddenAppManifest()
	}
	return nil
}

func scanAppManifestObject(decoder *json.Decoder) (bool, error) {
	seen := make(map[string]struct{})
	forbidden := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false, invalidAppManifest("manifest must be valid JSON")
		}
		key, ok := token.(string)
		if !ok {
			return false, invalidAppManifest("manifest must be valid JSON")
		}
		if _, exists := seen[key]; exists {
			return false, invalidAppManifest("manifest contains a duplicate field")
		}
		seen[key] = struct{}{}
		fieldForbidden := isForbiddenAppManifestField(key)
		if fieldForbidden {
			forbidden = true
		}
		nestedForbidden, err := scanAppManifestValue(decoder)
		if err != nil {
			return false, err
		}
		forbidden = forbidden || nestedForbidden
	}
	token, err := decoder.Token()
	if err != nil {
		return false, invalidAppManifest("manifest must be valid JSON")
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return false, invalidAppManifest("manifest must be valid JSON")
	}
	return forbidden, nil
}

func scanAppManifestValue(decoder *json.Decoder) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, invalidAppManifest("manifest must be valid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return false, nil
	}
	switch delim {
	case '{':
		return scanAppManifestObject(decoder)
	case '[':
		forbidden := false
		for decoder.More() {
			nestedForbidden, err := scanAppManifestValue(decoder)
			if err != nil {
				return false, err
			}
			forbidden = forbidden || nestedForbidden
		}
		token, err := decoder.Token()
		if err != nil {
			return false, invalidAppManifest("manifest must be valid JSON")
		}
		if closing, ok := token.(json.Delim); !ok || closing != ']' {
			return false, invalidAppManifest("manifest must be valid JSON")
		}
		return forbidden, nil
	default:
		return false, invalidAppManifest("manifest must be valid JSON")
	}
}

func validateAppManifestJSONSchema(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return invalidAppManifest("manifest must be valid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return invalidAppManifest("manifest must be one JSON object")
	}
	if err := validateAppManifestJSONObject(decoder, "root"); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateAppManifestJSONObject(decoder *json.Decoder, schemaName string) error {
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return invalidAppManifest("manifest must be valid JSON")
		}
		key, ok := token.(string)
		if !ok {
			return invalidAppManifest("manifest must be valid JSON")
		}
		fieldSchema, known := appManifestJSONObjectSchemas[schemaName][key]
		if !known {
			return invalidAppManifest("manifest does not match the v1 schema")
		}
		if err := validateAppManifestJSONValue(decoder, fieldSchema); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil {
		return invalidAppManifest("manifest must be valid JSON")
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return invalidAppManifest("manifest must be valid JSON")
	}
	return nil
}

func validateAppManifestJSONValue(
	decoder *json.Decoder,
	schema appManifestJSONFieldSchema,
) error {
	token, err := decoder.Token()
	if err != nil {
		return invalidAppManifest("manifest must be valid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		if schema.object == "" {
			return invalidAppManifest("manifest does not match the v1 schema")
		}
		return validateAppManifestJSONObject(decoder, schema.object)
	case '[':
		for decoder.More() {
			elementSchema := appManifestJSONFieldSchema{object: schema.arrayObject}
			if err := validateAppManifestJSONValue(decoder, elementSchema); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if err != nil {
			return invalidAppManifest("manifest must be valid JSON")
		}
		if closing, ok := token.(json.Delim); !ok || closing != ']' {
			return invalidAppManifest("manifest must be valid JSON")
		}
		return nil
	default:
		return invalidAppManifest("manifest must be valid JSON")
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		return invalidAppManifest("manifest must contain one JSON value")
	}
	return nil
}

func isForbiddenAppManifestField(field string) bool {
	normalized := normalizeAppManifestASCIIField(field)
	switch normalized {
	case "baseurl",
		"enabled",
		"enabledsurfaces",
		"allowedparentorigins",
		"alloweduserpolicy",
		"allowedorigins",
		"networkpolicy",
		"secretref",
		"privatekey",
		"privatekeyvalue",
		"apikey",
		"apikeyvalue",
		"clientsecret",
		"clientsecretvalue",
		"code",
		"sourcecode",
		"entitlementpolicy",
		"entitlementpolicyversion",
		"iframe",
		"iframeurl",
		"tokenvalue":
		return true
	}

	for _, credentialQualifier := range []string{
		"access",
		"api",
		"private",
		"secret",
		"client",
		"service",
		"signing",
		"encryption",
	} {
		if strings.Contains(normalized, credentialQualifier+"key") {
			return true
		}
	}

	for _, sensitive := range []string{
		"credential",
		"secret",
		"password",
		"passwd",
		"token",
		"apikey",
		"privatekey",
		"authorization",
		"bearer",
		"scripturl",
		"executable",
		"iframeurl",
		"proxy",
	} {
		if strings.Contains(normalized, sensitive) {
			return true
		}
	}

	segments := tokenizeAppManifestASCIIField(field)
	for index, segment := range segments {
		if segment == "script" ||
			segment == "auth" && index+1 < len(segments) && segments[index+1] == "header" {
			return true
		}
	}
	if normalized == "authheader" {
		return true
	}

	if strings.HasPrefix(normalized, "code") && normalized != "codec" {
		return true
	}
	for _, codeCombination := range []string{"authcode", "authorizationcode", "passcode", "accesscode"} {
		if strings.Contains(normalized, codeCombination) {
			return true
		}
	}
	return false
}

func normalizeAppManifestASCIIField(field string) string {
	var normalized strings.Builder
	normalized.Grow(len(field))
	for index := range len(field) {
		char := field[index]
		switch {
		case char >= 'a' && char <= 'z' || char >= '0' && char <= '9':
			normalized.WriteByte(char)
		case char >= 'A' && char <= 'Z':
			normalized.WriteByte(char + ('a' - 'A'))
		case char >= utf8.RuneSelf:
			return ""
		}
	}
	return normalized.String()
}

func tokenizeAppManifestASCIIField(field string) []string {
	segments := make([]string, 0, 4)
	start := -1
	appendSegment := func(end int) {
		if start >= 0 {
			segments = append(segments, strings.ToLower(field[start:end]))
			start = -1
		}
	}

	for index := range len(field) {
		char := field[index]
		if char >= utf8.RuneSelf {
			return nil
		}
		if !isAppManifestASCIIAlphaNumeric(char) {
			appendSegment(index)
			continue
		}
		if start < 0 {
			start = index
			continue
		}
		if char >= 'A' && char <= 'Z' {
			previous := field[index-1]
			nextIsLower := index+1 < len(field) && field[index+1] >= 'a' && field[index+1] <= 'z'
			if previous >= 'a' && previous <= 'z' ||
				previous >= '0' && previous <= '9' ||
				previous >= 'A' && previous <= 'Z' && nextIsLower {
				appendSegment(index)
				start = index
			}
		}
	}
	appendSegment(len(field))
	return segments
}

func isAppManifestASCIIAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' ||
		char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9'
}

func validAppManifest(manifest AppManifest) bool {
	if manifest.APIVersion != 1 ||
		manifest.Kind != "app" ||
		!appManifestSlugPattern.MatchString(manifest.Key) ||
		strings.TrimSpace(manifest.Name.EN) == "" ||
		strings.TrimSpace(manifest.Name.ZH) == "" ||
		!strictSemverPattern.MatchString(manifest.Version) ||
		!validAppManifestPath(manifest.CallbackPath) ||
		!validAppManifestPath(manifest.Surfaces.Direct.StartPath) ||
		!validAppManifestPath(manifest.Surfaces.Embedded.StartPath) ||
		len(manifest.RequestedScopes) == 0 ||
		len(manifest.Requires.TaskPlugins) == 0 {
		return false
	}

	scopes := make(map[string]struct{}, len(manifest.RequestedScopes))
	for _, scope := range manifest.RequestedScopes {
		if !allowedAppManifestScope(scope) {
			return false
		}
		if _, exists := scopes[scope]; exists {
			return false
		}
		scopes[scope] = struct{}{}
	}

	taskPlugins := make(map[string]struct{}, len(manifest.Requires.TaskPlugins))
	for _, taskPlugin := range manifest.Requires.TaskPlugins {
		if !appManifestSlugPattern.MatchString(taskPlugin.Key) ||
			!strictSemverPattern.MatchString(taskPlugin.MinimumVersion) {
			return false
		}
		if _, exists := taskPlugins[taskPlugin.Key]; exists {
			return false
		}
		taskPlugins[taskPlugin.Key] = struct{}{}
	}
	return true
}

func allowedAppManifestScope(scope string) bool {
	switch scope {
	case "identity.read",
		"model.invoke",
		"task.import",
		"task.read",
		"resource.reserve",
		"resource.meter":
		return true
	default:
		return false
	}
}

func validAppManifestPath(path string) bool {
	if !validAppManifestPathLayer(path) {
		return false
	}
	decodedPath, ok := decodeAppManifestPath(path)
	if !ok {
		return false
	}
	return validAppManifestPathLayer(decodedPath)
}

func decodeAppManifestPath(path string) (string, bool) {
	// Each round contracts disjoint %HH nodes; every contraction removes two
	// active nodes, so decoding to the fixpoint remains linear in input size.
	values := []byte(path)
	next := make([]int, len(values))
	percentNodes := make([]int, 0)
	for index, value := range values {
		next[index] = index + 1
		if value == '%' {
			percentNodes = append(percentNodes, index)
		}
	}
	next[len(next)-1] = -1

	for len(percentNodes) > 0 {
		for _, percent := range percentNodes {
			first := next[percent]
			if first < 0 {
				return "", false
			}
			second := next[first]
			if second < 0 || !isAppManifestHex(values[first]) || !isAppManifestHex(values[second]) {
				return "", false
			}
		}

		nextPercentNodes := make([]int, 0, len(percentNodes))
		for _, percent := range percentNodes {
			first := next[percent]
			second := next[first]
			values[percent] = appManifestHexValue(values[first])<<4 | appManifestHexValue(values[second])
			next[percent] = next[second]
			if values[percent] == '%' {
				nextPercentNodes = append(nextPercentNodes, percent)
			}
		}
		percentNodes = nextPercentNodes
	}

	decoded := make([]byte, 0, len(values))
	for index := 0; index >= 0; index = next[index] {
		decoded = append(decoded, values[index])
	}
	return string(decoded), true
}

func isAppManifestHex(value byte) bool {
	return value >= '0' && value <= '9' ||
		value >= 'a' && value <= 'f' ||
		value >= 'A' && value <= 'F'
}

func appManifestHexValue(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10
	default:
		return value - 'A' + 10
	}
}

func validAppManifestPathLayer(path string) bool {
	if path == "" ||
		path[0] != '/' ||
		strings.HasPrefix(path, "//") ||
		strings.ContainsAny(path, "?#") {
		return false
	}
	for _, char := range path {
		if char == '\\' || unicode.IsControl(char) {
			return false
		}
	}
	parsed, err := url.Parse(path)
	if err != nil ||
		parsed.IsAbs() ||
		parsed.Host != "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.ForceQuery ||
		parsed.Fragment != "" {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func invalidAppManifest(reason string) *AppManifestError {
	return &AppManifestError{
		Code:        AppManifestInvalidErrorCode,
		FieldErrors: []string{reason},
	}
}

func forbiddenAppManifest() *AppManifestError {
	return &AppManifestError{
		Code:        AppManifestForbiddenFieldErrorCode,
		FieldErrors: []string{"manifest contains a forbidden field"},
	}
}
