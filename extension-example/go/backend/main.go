package main

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	sdk "github.com/taills/moduless/sdk/go"
)

// collection is the CMDS collection declared in manifest.yaml.
const collection = "items"

// Item is the demo CRUD resource. Core provisions the "items" table and its
// indexes (status, unique code) from manifest.yaml.
type Item struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Code   string `json:"code"`
	Status string `json:"status"`
}

// Go extension example. Opens no port — the SDK dials Core's gRPC tunnel.
// Dev:  go run extension-example/go/backend/main.go
// Prod: set FRONTEND_DIR to the built dist so the SDK ships it to Core.
func main() {
	coreURL := env("CORE_URL", "localhost:9000")

	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/info", handleInfo)
	r.GET("/items", listItems)
	r.POST("/items", createItem)
	r.GET("/items/:id", getItem)
	r.PUT("/items/:id", updateItem)
	r.DELETE("/items/:id", deleteItem)

	cfg := sdk.Config{
		ExtensionKey: "go_example",
		CoreGrpcURL:  coreURL,
		ManifestPath: os.Getenv("MANIFEST_PATH"),
	}
	if dir := os.Getenv("FRONTEND_DIR"); dir != "" {
		cfg.FrontendDir = dir // production: Core serves the bundled micro-frontend
	} else {
		cfg.IsDev = true
		cfg.DevFEUrl = env("DEV_FE_URL", "http://localhost:7100")
	}

	sdk.Start(r, cfg)
}

func handleInfo(c *gin.Context) {
	user := sdk.GetUser(c.Request.Context())
	userID := "anonymous"
	var roles []string
	if user != nil {
		userID = user.UserID
		roles = user.Roles
	}
	host, _ := os.Hostname() // identifies which replica served the request
	c.JSON(http.StatusOK, gin.H{"language": "go", "user_id": userID, "roles": roles, "instance": host})
}

func listItems(c *gin.Context) {
	var filters []sdk.Filter
	if status := c.Query("status"); status != "" {
		filters = append(filters, sdk.Filter{Field: "status", Operator: "=", Value: status})
	}
	items := []Item{}
	err := sdk.DB.FindInto(c.Request.Context(), collection, filters,
		int32(atoiDefault(c.Query("limit"), 100)),
		int32(atoiDefault(c.Query("offset"), 0)), &items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "count": len(items)})
}

func createItem(c *gin.Context) {
	var item Item
	if err := c.ShouldBindJSON(&item); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if item.Name == "" || item.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and code are required"})
		return
	}
	if item.Status == "" {
		item.Status = "active"
	}
	item.ID = strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := sdk.DB.Put(c.Request.Context(), collection, item.ID, item); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, item)
}

func getItem(c *gin.Context) {
	var item Item
	found, err := sdk.DB.Get(c.Request.Context(), collection, c.Param("id"), &item)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, item)
}

func updateItem(c *gin.Context) {
	var item Item
	if err := c.ShouldBindJSON(&item); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if item.Name == "" || item.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and code are required"})
		return
	}
	if item.Status == "" {
		item.Status = "active"
	}
	item.ID = c.Param("id")
	if err := sdk.DB.Put(c.Request.Context(), collection, item.ID, item); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, item)
}

func deleteItem(c *gin.Context) {
	if err := sdk.DB.Delete(c.Request.Context(), collection, c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
