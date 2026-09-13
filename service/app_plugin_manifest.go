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
)

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
		if isForbiddenAppManifestField(key) {
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

func requireJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		return invalidAppManifest("manifest must contain one JSON value")
	}
	return nil
}

func isForbiddenAppManifestField(field string) bool {
	canonical := canonicalAppManifestField(field)
	switch canonical {
	case "baseurl",
		"enabled",
		"enabledsurfaces",
		"allowedparentorigins",
		"alloweduserpolicy",
		"allowedorigins",
		"networkpolicy",
		"secretref":
		return true
	}
	if strings.Contains(canonical, "credential") ||
		strings.Contains(canonical, "secret") ||
		strings.Contains(canonical, "privatekey") ||
		strings.Contains(canonical, "apikey") ||
		strings.Contains(canonical, "password") ||
		strings.Contains(canonical, "passwd") ||
		strings.Contains(canonical, "accesstoken") ||
		strings.Contains(canonical, "authtoken") ||
		strings.Contains(canonical, "bearertoken") ||
		strings.HasSuffix(canonical, "token") {
		return true
	}
	if strings.Contains(canonical, "script") ||
		strings.Contains(canonical, "executable") ||
		canonical == "code" ||
		strings.HasSuffix(canonical, "code") {
		return true
	}
	if strings.HasPrefix(canonical, "iframe") {
		return true
	}
	return strings.Contains(canonical, "proxy")
}

func canonicalAppManifestField(field string) string {
	var normalized strings.Builder
	normalized.Grow(len(field))
	for _, char := range field {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			normalized.WriteRune(unicode.ToLower(char))
		}
	}
	return normalized.String()
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
	if path == "" || path[0] != '/' || strings.HasPrefix(path, "//") {
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
	decodedPath := parsed.EscapedPath()
	for {
		next, err := url.PathUnescape(decodedPath)
		if err != nil {
			return false
		}
		if next == decodedPath {
			break
		}
		decodedPath = next
	}
	for _, char := range decodedPath {
		if char == '\\' || unicode.IsControl(char) {
			return false
		}
	}
	for _, segment := range strings.Split(decodedPath, "/") {
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
