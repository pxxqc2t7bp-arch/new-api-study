package dto

type ThreeDUsage struct {
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ThreeDError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type ThreeDGeneration struct {
	ID        string       `json:"id"`
	Object    string       `json:"object,omitempty"`
	Model     string       `json:"model,omitempty"`
	Status    string       `json:"status,omitempty"`
	FileURL   string       `json:"file_url,omitempty"`
	Usage     *ThreeDUsage `json:"usage,omitempty"`
	Error     *ThreeDError `json:"error,omitempty"`
	CreatedAt int64        `json:"created_at,omitempty"`
	UpdatedAt int64        `json:"updated_at,omitempty"`
	ExpiresAt int64        `json:"expires_at,omitempty"`
}
