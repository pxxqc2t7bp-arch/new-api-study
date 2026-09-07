package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func SetBatchRouter(router *gin.Engine) {
	files := router.Group("/v1/files")
	files.Use(middleware.RouteTag("relay"), middleware.SystemPerformanceCheck(), middleware.TokenAuth())
	{
		files.POST("", controller.CreateAPIFile)
		files.GET("", controller.ListAPIFiles)
		files.GET("/:id", controller.RetrieveAPIFile)
		files.DELETE("/:id", controller.DeleteAPIFile)
		files.GET("/:id/content", controller.DownloadAPIFile)
		files.HEAD("/:id/content", controller.DownloadAPIFile)
	}

	batches := router.Group("/v1/batches")
	batches.Use(middleware.RouteTag("relay"), middleware.SystemPerformanceCheck(), middleware.TokenAuth())
	{
		batches.POST("", controller.CreateBatch)
		batches.GET("", controller.ListBatches)
		batches.GET("/:id", controller.RetrieveBatch)
		batches.POST("/:id/cancel", controller.CancelBatch)
	}
}
