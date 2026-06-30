package main

import (
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	sdk "github.com/taills/moduless/sdk/go"
)

// Go extension example. Opens no port — the SDK dials Core's gRPC tunnel.
// Run: go run extension-example/go/backend/main.go
func main() {
	coreURL := os.Getenv("CORE_URL")
	if coreURL == "" {
		coreURL = "localhost:9000"
	}

	r := gin.New()

	r.GET("/info", func(c *gin.Context) {
		user := sdk.GetUser(c.Request.Context())
		userID := "anonymous"
		var roles []string
		if user != nil {
			userID = user.UserID
			roles = user.Roles
		}
		c.JSON(http.StatusOK, gin.H{
			"language": "go",
			"user_id":  userID,
			"roles":    roles,
		})
	})

	r.POST("/items/:id", func(c *gin.Context) {
		var payload map[string]any
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := sdk.DB.Put(c.Request.Context(), "items", c.Param("id"), payload); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": c.Param("id")})
	})

	r.GET("/items/:id", func(c *gin.Context) {
		var item map[string]any
		found, err := sdk.DB.Get(c.Request.Context(), "items", c.Param("id"), &item)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, item)
	})

	sdk.Start(r, sdk.Config{
		ExtensionKey: "go_example",
		CoreGrpcURL:  coreURL,
		IsDev:        true,
		ManifestPath: os.Getenv("MANIFEST_PATH"),
	})
}
