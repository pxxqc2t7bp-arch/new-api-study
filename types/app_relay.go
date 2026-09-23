package types

const (
	AppRelaySubjectContextKey  = "app_relay_subject"
	AppTaskRetrievalContextKey = "app_task_retrieval"
)

type AppRelayAssetInput struct {
	MediaType string
	AssetURI  string
	ExpiresAt int64
}

// AppRelaySubject carries a verified, secret-free identity across the relay
// packages. Authority is reloaded in the transaction that wins dispatch.
type AppRelaySubject struct {
	GrantID          string
	TokenHash        string
	UserID           int
	Protocol         string
	PublicModel      string
	ExecutionKind    string
	SubmissionHash   string
	ChannelID        int
	Group            string
	Strategy         string
	ConversionPolicy string
	PluginSHA256     string
	AssetInputs      []AppRelayAssetInput
}
