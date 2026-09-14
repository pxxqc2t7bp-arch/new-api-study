package router

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerAppPluginRoutes(apiRouter *gin.RouterGroup) {
	apiRouter.GET("/app_plugins", middleware.UserAuth(), controller.ListAppPlugins)

	installations := apiRouter.Group("/app_plugins/installations")
	installations.Use(middleware.UserAuth(), requireAppPluginManage())
	{
		installations.GET("", controller.ListAppPluginInstallations)
		installations.POST("", middleware.AppPluginOperationAudit(), controller.CreateAppPluginInstallation)
		installations.PATCH("", middleware.AppPluginOperationAudit(), controller.PatchAppPluginInstallation)
	}
}

func requireAppPluginManage() gin.HandlerFunc {
	return func(c *gin.Context) {
		if authz.Can(c.GetInt("id"), c.GetInt("role"), authz.AppPluginManage) {
			c.Next()
			return
		}
		envelope := authz.NewAppPluginError(
			authz.AppPluginErrorCodePermissionDenied,
			c.GetString(common.RequestIdKey),
			nil,
		)
		c.AbortWithStatusJSON(envelope.StatusCode, envelope)
	}
}
