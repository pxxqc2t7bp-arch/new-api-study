package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func SetThreeDRouter(router *gin.Engine) {
	threeD := router.Group("/v1/3d")
	threeD.Use(middleware.RouteTag("relay"))
	{
		submit := threeD.Group("")
		submit.Use(middleware.TokenAuth(), middleware.Distribute())
		submit.POST("/generations", controller.RelayTask)

		task := threeD.Group("")
		task.Use(middleware.TokenAuth())
		task.GET("/generations/:task_id", controller.RelayThreeDFetch)
		task.DELETE("/generations/:task_id", controller.RelayThreeDCancel)
	}

	native := router.Group("/api/v3/contents/generations")
	native.Use(middleware.RouteTag("relay"))
	native.Use(middleware.VolcEngine3DRequestConvert())
	{
		submit := native.Group("")
		submit.Use(middleware.TokenAuth(), middleware.Distribute())
		submit.POST("/tasks", controller.RelayTask)

		task := native.Group("")
		task.Use(middleware.TokenAuth())
		task.GET("/tasks/:task_id", controller.RelayThreeDFetch)
		task.DELETE("/tasks/:task_id", controller.RelayThreeDCancel)
	}
}
