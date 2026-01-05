package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// ImageSearchItem represents a single image in the search response.
type ImageSearchItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Stars       int    `json:"stars"`
	Official    bool   `json:"official"`
	Index       string `json:"index"`
}

// ImageSearchResponse represents the API response for image search.
type ImageSearchResponse struct {
	Images []ImageSearchItem `json:"images"`
	Query  string            `json:"query"`
}

// handleImageSearch handles GET /api/v1/images/search?q=<query>&limit=25
func (s *GinServer) handleImageSearch(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		writeGinError(c, http.StatusBadRequest, "Missing required query parameter: q")
		return
	}

	limit := 25
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	results, err := s.appManager.SearchImages(c.Request.Context(), query, limit)
	if err != nil {
		writeGinError(c, http.StatusInternalServerError, "Search failed: "+err.Error())
		return
	}

	items := make([]ImageSearchItem, 0, len(results))
	for _, r := range results {
		items = append(items, ImageSearchItem{
			Name:        r.Name,
			Description: r.Description,
			Stars:       r.Stars,
			Official:    r.Official == "[OK]",
			Index:       r.Index,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data": ImageSearchResponse{
			Images: items,
			Query:  query,
		},
	})
}
