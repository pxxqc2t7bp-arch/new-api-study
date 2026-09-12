package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

func registerRealtimeTicketRoutes(apiRouter *gin.RouterGroup) {
	apiRouter.POST(
		"/realtime/tickets",
		middleware.UserAuth(),
		middleware.UserCriticalRateLimit("realtime-ticket"),
		middleware.DisableCache(),
		controller.IssueRealtimeTicket,
	)
}

func registerRealtimeRelayRoute(router *gin.Engine) {
	realtimeRouter := router.Group("/v1")
	realtimeRouter.Use(middleware.RouteTag("relay"))
	realtimeRouter.Use(middleware.SystemPerformanceCheck())
	realtimeRouter.Use(middleware.RealtimeAuth())
	realtimeRouter.Use(middleware.ModelRequestRateLimit(), middleware.Distribute())
	realtimeRouter.GET("/realtime", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIRealtime)
	})

	geminiLiveRouter := router.Group("")
	geminiLiveRouter.Use(middleware.RouteTag("relay"))
	geminiLiveRouter.Use(middleware.SystemPerformanceCheck())
	geminiLiveRouter.GET(
		middleware.GeminiLivePath,
		middleware.RealtimeAuth(),
		middleware.GeminiLiveSetup(),
		middleware.ModelRequestRateLimit(),
		middleware.Distribute(),
		func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatGeminiLive)
		},
	)
}
