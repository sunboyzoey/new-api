package controller

import (
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"net/http"
)

func GetGroupCacheHealth(c *gin.Context) {
	report, err := service.GetGroupCacheReport(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Cache statistics unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": report})
}
