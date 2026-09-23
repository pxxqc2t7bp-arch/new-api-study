package operation_setting

const (
	AppPluginV1EnabledOptionKey              = "APP_PLUGIN_V1_ENABLED"
	AppPluginSeedanceEnabledOptionKey        = "APP_PLUGIN_SEEDANCE_ENABLED"
	AppPluginEmbeddedSurfaceEnabledOptionKey = "APP_PLUGIN_EMBEDDED_SURFACE_ENABLED"
	AppExecutionGrantsEnabledOptionKey       = "APP_EXECUTION_GRANTS_ENABLED"
)

var (
	AppPluginV1Enabled              = false
	AppPluginSeedanceEnabled        = false
	AppPluginEmbeddedSurfaceEnabled = false
	AppExecutionGrantsEnabled       = false
)

func IsAppPluginFeatureFlag(key string) bool {
	switch key {
	case AppPluginV1EnabledOptionKey,
		AppPluginSeedanceEnabledOptionKey,
		AppPluginEmbeddedSurfaceEnabledOptionKey,
		AppExecutionGrantsEnabledOptionKey:
		return true
	default:
		return false
	}
}
