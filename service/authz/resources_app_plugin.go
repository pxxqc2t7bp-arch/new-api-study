package authz

import "net/http"

const (
	ResourceAppPlugin = "app_plugin"
	ActionManage      = "manage"
)

var AppPluginManage = Permission{Resource: ResourceAppPlugin, Action: ActionManage}

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceAppPlugin,
		LabelKey: "App Plugin",
		Actions: []ActionDefinition{{
			Action:         ActionManage,
			LabelKey:       "Manage app plugins",
			DescriptionKey: "Create, update, publish, and disable app plugins.",
			DefaultRoles:   []string{BuiltInRolePluginAdmin},
		}},
	})
}

type AppPluginErrorCode string

const (
	AppPluginErrorCodeFeatureDisabled  AppPluginErrorCode = "app_plugin_disabled"
	AppPluginErrorCodeInvalidRequest   AppPluginErrorCode = "invalid_request"
	AppPluginErrorCodePermissionDenied AppPluginErrorCode = "not_found"
	AppPluginErrorCodeNotFound         AppPluginErrorCode = "not_found"
	AppPluginErrorCodeConflict         AppPluginErrorCode = "app_version_conflict"
	AppPluginErrorCodeInternal         AppPluginErrorCode = "service_unavailable"
)

type AppPluginErrorEnvelope struct {
	StatusCode int            `json:"-"`
	Error      AppPluginError `json:"error"`
}

type AppPluginFieldError struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

type AppPluginError struct {
	Code        AppPluginErrorCode    `json:"code"`
	Message     string                `json:"message"`
	FieldErrors []AppPluginFieldError `json:"field_errors"`
	Retryable   bool                  `json:"retryable"`
	RequestID   string                `json:"request_id"`
}

// NewAppPluginError returns only public, stable error text. The private cause is
// deliberately excluded so credentials and object identifiers cannot leak.
func NewAppPluginError(code AppPluginErrorCode, requestID string, _ error) AppPluginErrorEnvelope {
	message := ""
	retryable := false
	statusCode := http.StatusServiceUnavailable
	switch code {
	case AppPluginErrorCodeFeatureDisabled:
		message = "App plugin API is disabled"
		statusCode = http.StatusForbidden
	case AppPluginErrorCodeInvalidRequest:
		message = "Invalid app plugin request"
		statusCode = http.StatusBadRequest
	case AppPluginErrorCodeNotFound:
		message = "App plugin not found"
		statusCode = http.StatusNotFound
	case AppPluginErrorCodeConflict:
		message = "App plugin state conflict"
		statusCode = http.StatusConflict
	case AppPluginErrorCodeInternal:
		message = "App plugin request failed"
		retryable = true
	default:
		code = AppPluginErrorCodeInternal
		message = "App plugin request failed"
		retryable = true
	}
	return AppPluginErrorEnvelope{
		StatusCode: statusCode,
		Error: AppPluginError{
			Code:        code,
			Message:     message,
			FieldErrors: []AppPluginFieldError{},
			Retryable:   retryable,
			RequestID:   requestID,
		},
	}
}
