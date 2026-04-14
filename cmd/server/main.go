package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/screenshot-mcp-server/internal/chrome"
	"github.com/screenshot-mcp-server/internal/screenshot"
	"github.com/screenshot-mcp-server/internal/ws"
	"github.com/screenshot-mcp-server/pkg/types"
	"go.uber.org/zap"
)

// Server represents the MCP screenshot server
type Server struct {
	engine         types.ScreenshotEngine
	processor      types.ImageProcessor
	chromeManager  types.ChromeManager
	streamManager  *ws.StreamManager
	logger         *zap.Logger
	router         *gin.Engine
	httpServer     *http.Server
	config         *Config
	upgrader       websocket.Upgrader
}

// Config holds server configuration
type Config struct {
	Port           int    `json:"port"`
	Host           string `json:"host"`
	DefaultFormat  string `json:"default_format"`
	Quality        int    `json:"quality"`
	IncludeCursor  bool   `json:"include_cursor"`
	LogLevel       string `json:"log_level"`
	ChromeTimeout  string `json:"chrome_timeout"`
	// WebSocket streaming configuration
	StreamMaxSessions int `json:"stream_max_sessions"`
	StreamDefaultFPS  int `json:"stream_default_fps"`
}

// DefaultConfig returns default server configuration
func DefaultConfig() *Config {
	return &Config{
		Port:              8080,
		Host:              "127.0.0.1",
		DefaultFormat:     "png",
		Quality:           95,
		IncludeCursor:     false,
		LogLevel:          "info",
		ChromeTimeout:     "30s",
		StreamMaxSessions: 10,
		StreamDefaultFPS:  10,
	}
}

// NewServer creates a new screenshot server
func NewServer() (*Server, error) {
	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	// Initialize screenshot engine
	engine, err := screenshot.NewEngine()
	if err != nil {
		logger.Error("Failed to create screenshot engine", zap.Error(err))
		return nil, fmt.Errorf("failed to create screenshot engine: %w", err)
	}

	// Initialize Chrome manager
	chromeManager := chrome.NewManager()

	// Initialize stream manager
	streamManager := ws.NewStreamManager(logger)

	// Create WebSocket upgrader
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins for now
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	// Create server instance
	server := &Server{
		engine:        engine,
		processor:     screenshot.NewImageProcessor(),
		chromeManager: chromeManager,
		streamManager: streamManager,
		logger:        logger,
		config:        DefaultConfig(),
		upgrader:      upgrader,
	}

	// Setup HTTP router
	server.setupRouter()

	return server, nil
}

// setupRouter configures the HTTP routes
func (s *Server) setupRouter() {
	// Use gin in release mode for production
	gin.SetMode(gin.ReleaseMode)
	
	s.router = gin.New()
	
	// Middleware
	s.router.Use(gin.Recovery())
	s.router.Use(s.loggingMiddleware())
	s.router.Use(s.corsMiddleware())

	// Health check
	s.router.GET("/health", s.healthCheck)

	// API v1 routes
	v1 := s.router.Group("/v1")
	{
		// Screenshot endpoints
		v1.POST("/screenshot", s.takeScreenshot)
		v1.GET("/screenshot", s.takeScreenshotGET)
		
		// Window management
		v1.GET("/windows", s.listWindows)
		v1.GET("/windows/:handle", s.getWindow)
		
		// Chrome integration
		v1.GET("/chrome/instances", s.listChromeInstances)
		v1.GET("/chrome/tabs", s.listChromeTabs)
		v1.POST("/chrome/tabs/:id/screenshot", s.takeChromeTabScreenshot)
		
		// WebSocket streaming
		v1.GET("/stream/:windowId", s.handleWebSocketStream)
		v1.GET("/stream/status", s.getStreamStatus)
	}

	// API routes (for compatibility)
	api := s.router.Group("/api")
	{
		api.GET("/health", s.healthCheck)
		api.GET("/windows", s.listWindows)
		api.GET("/screenshot", s.takeScreenshotGET)
	}

	// WebSocket streaming routes (top level for simplicity)
	s.router.GET("/stream/:windowId", s.handleWebSocketStream)

	// MCP JSON-RPC 2.0 endpoint (legacy custom protocol)
	s.router.POST("/rpc", s.handleMCPRequest)

	// Standard MCP protocol endpoint (Streamable HTTP transport)
	s.router.POST("/mcp", s.handleMCPProtocol)

	// Documentation
	s.router.Static("/docs", "./docs")
	s.router.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/docs")
	})
}

// Start starts the HTTP server
func (s *Server) Start() error {
	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.config.Host, s.config.Port),
		Handler: s.router,
	}

	s.logger.Info("Starting screenshot MCP server",
		zap.String("address", s.httpServer.Addr),
		zap.String("version", "1.0.0"),
	)

	// Start server in a goroutine
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Fatal("Failed to start server", zap.Error(err))
		}
	}()

	// Wait for interrupt signal to gracefully shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	s.logger.Info("Shutting down server...")

	// Shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.logger.Error("Server forced to shutdown", zap.Error(err))
		return err
	}

	s.logger.Info("Server exited")
	return nil
}

// HTTP Handlers

// healthCheck returns server health status
func (s *Server) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now(),
		"version":   "1.0.0",
	})
}

// takeScreenshot handles screenshot requests
func (s *Server) takeScreenshot(c *gin.Context) {
	var req types.ScreenshotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	s.processScreenshotRequest(c, &req)
}

// takeScreenshotGET handles GET screenshot requests
func (s *Server) takeScreenshotGET(c *gin.Context) {
	req := types.ScreenshotRequest{
		Method:  c.DefaultQuery("method", "title"),
		Target:  c.Query("target"),
		Format:  types.ImageFormat(c.DefaultQuery("format", s.config.DefaultFormat)),
		Quality: s.config.Quality,
	}

	if qualityStr := c.Query("quality"); qualityStr != "" {
		if quality, err := strconv.Atoi(qualityStr); err == nil {
			req.Quality = quality
		}
	}

	req.IncludeCursor = c.Query("cursor") == "true"

	if req.Target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target parameter is required"})
		return
	}

	s.processScreenshotRequest(c, &req)
}

// processScreenshotRequest processes a screenshot request
func (s *Server) processScreenshotRequest(c *gin.Context, req *types.ScreenshotRequest) {
	startTime := time.Now()

	options := &types.CaptureOptions{
		IncludeCursor:    req.IncludeCursor,
		IncludeFrame:     true,
		ScaleFactor:      1.0,
		AllowMinimized:   true,
		RestoreWindow:    false,
		WaitForVisible:   2 * time.Second,
		RetryCount:       3,
		CustomProperties: make(map[string]string),
	}

	if req.Region != nil {
		options.Region = req.Region
	}

	var buffer *types.ScreenshotBuffer
	var err error

	// Capture based on method
	switch req.Method {
	case "title":
		buffer, err = s.engine.CaptureByTitle(req.Target, options)
	case "pid":
		if pid, parseErr := strconv.ParseUint(req.Target, 10, 32); parseErr == nil {
			buffer, err = s.engine.CaptureByPID(uint32(pid), options)
		} else {
			err = fmt.Errorf("invalid PID: %s", req.Target)
		}
	case "handle":
		if handle, parseErr := strconv.ParseUint(req.Target, 10, 64); parseErr == nil {
			buffer, err = s.engine.CaptureByHandle(uintptr(handle), options)
		} else {
			err = fmt.Errorf("invalid handle: %s", req.Target)
		}
	case "class":
		buffer, err = s.engine.CaptureByClassName(req.Target, options)
	default:
		err = fmt.Errorf("unsupported method: %s", req.Method)
	}

	if err != nil {
		s.logger.Error("Screenshot capture failed",
			zap.String("method", req.Method),
			zap.String("target", req.Target),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Encode the image data as base64
	imageFormat := req.Format
	if imageFormat == "" {
		imageFormat = types.FormatPNG
	}
	encoded, err := s.processor.Encode(buffer, imageFormat, req.Quality)
	if err != nil {
		s.logger.Error("Screenshot encoding failed",
			zap.String("method", req.Method),
			zap.String("target", req.Target),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	imageData := base64.StdEncoding.EncodeToString(encoded)

	response := types.ScreenshotResponse{
		Success:   true,
		Data:      imageData,
		Format:    string(imageFormat),
		Width:     buffer.Width,
		Height:    buffer.Height,
		Size:      int64(len(encoded)),
		Timestamp: buffer.Timestamp,
		Metadata: types.Metadata{
			CaptureMethod:  req.Method,
			ProcessingTime: time.Since(startTime),
			WindowVisible:  buffer.WindowInfo.IsVisible,
			WindowMinimized: buffer.WindowInfo.State == "minimized",
			DPIScaling:     float64(buffer.DPI) / 96.0,
			ColorDepth:     32,
			Properties:     options.CustomProperties,
		},
	}

	s.logger.Info("Screenshot captured successfully",
		zap.String("method", req.Method),
		zap.String("target", req.Target),
		zap.Int("width", buffer.Width),
		zap.Int("height", buffer.Height),
		zap.Duration("processing_time", response.Metadata.ProcessingTime),
	)

	c.JSON(http.StatusOK, response)
}

// listWindows lists all available windows
func (s *Server) listWindows(c *gin.Context) {
	// For now return a placeholder - window enumeration can be implemented later
	c.JSON(http.StatusOK, gin.H{
		"windows": []interface{}{},
		"message": "Window enumeration will be implemented in a future version",
	})
}

// getWindow gets information about a specific window
func (s *Server) getWindow(c *gin.Context) {
	handle := c.Param("handle")
	c.JSON(http.StatusOK, gin.H{
		"handle":  handle,
		"message": "Window details not yet implemented",
	})
}

// listChromeInstances lists all Chrome instances
func (s *Server) listChromeInstances(c *gin.Context) {
	instances, err := s.chromeManager.DiscoverInstances()
	if err != nil {
		s.logger.Error("Failed to discover Chrome instances", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"instances": instances,
		"count":     len(instances),
	})
}

// listChromeTabs lists tabs for all or specific Chrome instances
func (s *Server) listChromeTabs(c *gin.Context) {
	instances, err := s.chromeManager.DiscoverInstances()
	if err != nil {
		s.logger.Error("Failed to discover Chrome instances", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var allTabs []types.ChromeTab
	for _, instance := range instances {
		tabs, err := s.chromeManager.GetTabs(&instance)
		if err != nil {
			s.logger.Warn("Failed to get tabs for Chrome instance",
				zap.Uint32("pid", instance.PID),
				zap.Error(err),
			)
			continue
		}
		allTabs = append(allTabs, tabs...)
	}

	c.JSON(http.StatusOK, gin.H{
		"tabs":  allTabs,
		"count": len(allTabs),
	})
}

// takeChromeTabScreenshot takes a screenshot of a specific Chrome tab
func (s *Server) takeChromeTabScreenshot(c *gin.Context) {
	tabID := c.Param("id")

	// Find the tab
	instances, err := s.chromeManager.DiscoverInstances()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var targetTab *types.ChromeTab
	for _, instance := range instances {
		tabs, err := s.chromeManager.GetTabs(&instance)
		if err != nil {
			continue
		}
		
		for _, tab := range tabs {
			if tab.ID == tabID {
				targetTab = &tab
				break
			}
		}
		if targetTab != nil {
			break
		}
	}

	if targetTab == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Tab not found"})
		return
	}

	// Capture screenshot
	options := types.DefaultCaptureOptions()
	buffer, err := s.chromeManager.CaptureTab(targetTab, options)
	if err != nil {
		s.logger.Error("Failed to capture Chrome tab screenshot",
			zap.String("tab_id", tabID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Encode as base64
	encoded, err := s.processor.Encode(buffer, types.FormatPNG, s.config.Quality)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	imageData := base64.StdEncoding.EncodeToString(encoded)

	response := types.ScreenshotResponse{
		Success:   true,
		Data:      imageData,
		Format:    string(types.FormatPNG),
		Width:     buffer.Width,
		Height:    buffer.Height,
		Size:      int64(len(encoded)),
		Timestamp: buffer.Timestamp,
		Metadata: types.Metadata{
			CaptureMethod: "chrome_tab",
			Properties: map[string]string{
				"tab_id":    tabID,
				"tab_title": targetTab.Title,
				"tab_url":   targetTab.URL,
			},
		},
	}

	c.JSON(http.StatusOK, response)
}

// handleMCPRequest handles MCP JSON-RPC 2.0 requests
func (s *Server) handleMCPRequest(c *gin.Context) {
	var req types.MCPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		s.sendMCPError(c, nil, -32700, "Parse error", nil)
		return
	}

	s.logger.Debug("Received MCP request",
		zap.String("method", req.Method),
		zap.Any("id", req.ID),
	)

	switch req.Method {
	case "screenshot.capture":
		s.handleMCPScreenshot(c, &req)
	case "window.list":
		s.handleMCPWindowList(c, &req)
	case "chrome.instances":
		s.handleMCPChromeInstances(c, &req)
	case "chrome.tabs":
		s.handleMCPChromeTabs(c, &req)
	case "chrome.tabCapture":
		s.handleMCPChromeTabCapture(c, &req)
	case "stream.status":
		s.handleMCPStreamStatus(c, &req)
	default:
		s.sendMCPError(c, req.ID, -32601, "Method not found", nil)
	}
}

// handleMCPScreenshot handles MCP screenshot requests
func (s *Server) handleMCPScreenshot(c *gin.Context, req *types.MCPRequest) {
	params, ok := req.Params.(map[string]interface{})
	if !ok {
		s.sendMCPError(c, req.ID, -32602, "Invalid params", nil)
		return
	}

	screenshotReq := types.ScreenshotRequest{
		Method:        getString(params, "method", "title"),
		Target:        getString(params, "target", ""),
		Format:        types.ImageFormat(getString(params, "format", s.config.DefaultFormat)),
		Quality:       getInt(params, "quality", s.config.Quality),
		IncludeCursor: getBool(params, "include_cursor", s.config.IncludeCursor),
	}

	if screenshotReq.Target == "" {
		s.sendMCPError(c, req.ID, -32602, "Missing required parameter: target", nil)
		return
	}

	options := &types.CaptureOptions{
		IncludeCursor:    screenshotReq.IncludeCursor,
		IncludeFrame:     getBool(params, "include_frame", true),
		ScaleFactor:      getFloat64(params, "scale_factor", 1.0),
		AllowMinimized:   getBool(params, "allow_minimized", true),
		RestoreWindow:    getBool(params, "restore_window", false),
		WaitForVisible:   2 * time.Second,
		RetryCount:       3,
		CustomProperties: make(map[string]string),
	}

	var buffer *types.ScreenshotBuffer
	var err error

	switch screenshotReq.Method {
	case "title":
		buffer, err = s.engine.CaptureByTitle(screenshotReq.Target, options)
	case "pid":
		if pid, parseErr := strconv.ParseUint(screenshotReq.Target, 10, 32); parseErr == nil {
			buffer, err = s.engine.CaptureByPID(uint32(pid), options)
		} else {
			err = fmt.Errorf("invalid PID: %s", screenshotReq.Target)
		}
	case "handle":
		if handle, parseErr := strconv.ParseUint(screenshotReq.Target, 10, 64); parseErr == nil {
			buffer, err = s.engine.CaptureByHandle(uintptr(handle), options)
		} else {
			err = fmt.Errorf("invalid handle: %s", screenshotReq.Target)
		}
	case "class":
		buffer, err = s.engine.CaptureByClassName(screenshotReq.Target, options)
	default:
		err = fmt.Errorf("unsupported method: %s", screenshotReq.Method)
	}

	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Internal error", err.Error())
		return
	}

	format := screenshotReq.Format
	if format == "" {
		format = types.FormatPNG
	}
	encoded, err := s.processor.Encode(buffer, format, screenshotReq.Quality)
	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Encoding failed", err.Error())
		return
	}

	imageData := base64.StdEncoding.EncodeToString(encoded)
	result := types.ScreenshotResponse{
		Success:   true,
		Data:      imageData,
		Format:    string(format),
		Width:     buffer.Width,
		Height:    buffer.Height,
		Size:      int64(len(encoded)),
		Timestamp: buffer.Timestamp,
	}

	s.sendMCPResult(c, req.ID, result)
}

// handleMCPWindowList handles MCP window list requests
func (s *Server) handleMCPWindowList(c *gin.Context, req *types.MCPRequest) {
	// Placeholder implementation
	result := map[string]interface{}{
		"windows": []interface{}{},
		"message": "Window enumeration not yet implemented",
	}
	s.sendMCPResult(c, req.ID, result)
}

// handleMCPChromeInstances handles MCP Chrome instances requests
func (s *Server) handleMCPChromeInstances(c *gin.Context, req *types.MCPRequest) {
	instances, err := s.chromeManager.DiscoverInstances()
	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Internal error", err.Error())
		return
	}

	result := map[string]interface{}{
		"instances": instances,
		"count":     len(instances),
	}
	s.sendMCPResult(c, req.ID, result)
}

// handleMCPChromeTabs handles MCP Chrome tabs requests
func (s *Server) handleMCPChromeTabs(c *gin.Context, req *types.MCPRequest) {
	instances, err := s.chromeManager.DiscoverInstances()
	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Internal error", err.Error())
		return
	}

	var allTabs []types.ChromeTab
	for _, instance := range instances {
		tabs, err := s.chromeManager.GetTabs(&instance)
		if err != nil {
			continue
		}
		allTabs = append(allTabs, tabs...)
	}

	result := map[string]interface{}{
		"tabs":  allTabs,
		"count": len(allTabs),
	}
	s.sendMCPResult(c, req.ID, result)
}

// handleMCPChromeTabCapture handles MCP Chrome tab capture requests
func (s *Server) handleMCPChromeTabCapture(c *gin.Context, req *types.MCPRequest) {
	params, ok := req.Params.(map[string]interface{})
	if !ok {
		s.sendMCPError(c, req.ID, -32602, "Invalid params", nil)
		return
	}

	tabID := getString(params, "tab_id", "")
	if tabID == "" {
		s.sendMCPError(c, req.ID, -32602, "Missing required parameter: tab_id", nil)
		return
	}

	instances, err := s.chromeManager.DiscoverInstances()
	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Internal error", err.Error())
		return
	}

	var targetTab *types.ChromeTab
	for _, instance := range instances {
		tabs, err := s.chromeManager.GetTabs(&instance)
		if err != nil {
			continue
		}
		for _, tab := range tabs {
			if tab.ID == tabID {
				targetTab = &tab
				break
			}
		}
		if targetTab != nil {
			break
		}
	}

	if targetTab == nil {
		s.sendMCPError(c, req.ID, -32603, "Tab not found", nil)
		return
	}

	options := types.DefaultCaptureOptions()
	buffer, err := s.chromeManager.CaptureTab(targetTab, options)
	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Screenshot failed", err.Error())
		return
	}

	encoded, err := s.processor.Encode(buffer, types.FormatPNG, s.config.Quality)
	if err != nil {
		s.sendMCPError(c, req.ID, -32603, "Encoding failed", err.Error())
		return
	}

	imageData := base64.StdEncoding.EncodeToString(encoded)
	result := types.ScreenshotResponse{
		Success:   true,
		Data:      imageData,
		Format:    string(types.FormatPNG),
		Width:     buffer.Width,
		Height:    buffer.Height,
		Size:      int64(len(encoded)),
		Timestamp: buffer.Timestamp,
	}

	s.sendMCPResult(c, req.ID, result)
}

// MCP helper functions

func (s *Server) sendMCPResult(c *gin.Context, id interface{}, result interface{}) {
	response := types.MCPResponse{
		JSONRPC: "2.0",
		Result:  result,
		ID:      id,
	}
	c.JSON(http.StatusOK, response)
}

func (s *Server) sendMCPError(c *gin.Context, id interface{}, code int, message string, data interface{}) {
	response := types.MCPResponse{
		JSONRPC: "2.0",
		Error: &types.MCPError{
			Code:    code,
			Message: message,
			Data:    data,
		},
		ID: id,
	}
	c.JSON(http.StatusOK, response) // MCP errors are still HTTP 200
}

// Parameter parsing helpers
func getString(params map[string]interface{}, key string, defaultValue string) string {
	if val, exists := params[key]; exists {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return defaultValue
}

func getInt(params map[string]interface{}, key string, defaultValue int) int {
	if val, exists := params[key]; exists {
		switch v := val.(type) {
		case int:
			return v
		case float64:
			return int(v)
		case string:
			if i, err := strconv.Atoi(v); err == nil {
				return i
			}
		}
	}
	return defaultValue
}

func getBool(params map[string]interface{}, key string, defaultValue bool) bool {
	if val, exists := params[key]; exists {
		if b, ok := val.(bool); ok {
			return b
		}
	}
	return defaultValue
}

func getFloat64(params map[string]interface{}, key string, defaultValue float64) float64 {
	if val, exists := params[key]; exists {
		switch v := val.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return defaultValue
}

// Middleware

func (s *Server) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		// Process request
		c.Next()

		// Log request
		latency := time.Since(start)
		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()

		if raw != "" {
			path = path + "?" + raw
		}

		s.logger.Info("HTTP Request",
			zap.String("client_ip", clientIP),
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("status", statusCode),
			zap.Duration("latency", latency),
		)
	}
}

func (s *Server) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// handleMCPStreamStatus handles MCP stream status requests
func (s *Server) handleMCPStreamStatus(c *gin.Context, req *types.MCPRequest) {
	stats := s.streamManager.GetStats()
	result := map[string]interface{}{
		"active_sessions": stats.ActiveSessions,
		"total_sessions":  stats.TotalSessions,
		"total_frames":    stats.TotalFrames,
		"uptime":          stats.Uptime.String(),
		"max_sessions":    s.config.StreamMaxSessions,
		"websocket_url":   fmt.Sprintf("ws://%s:%d/stream/{windowId}", s.config.Host, s.config.Port),
	}
	s.sendMCPResult(c, req.ID, result)
}

// WebSocket streaming handlers

// handleWebSocketStream handles WebSocket streaming connections
func (s *Server) handleWebSocketStream(c *gin.Context) {
	windowIDStr := c.Param("windowId")
	windowID, err := strconv.Atoi(windowIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid window ID"})
		return
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := s.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		s.logger.Error("WebSocket upgrade failed", zap.Error(err))
		return
	}
	defer conn.Close()

	// Parse query parameters for initial options
	fps := s.config.StreamDefaultFPS
	quality := s.config.Quality
	format := s.config.DefaultFormat

	if fpsStr := c.Query("fps"); fpsStr != "" {
		if f, err := strconv.Atoi(fpsStr); err == nil && f > 0 && f <= 60 {
			fps = f
		}
	}

	if qualityStr := c.Query("quality"); qualityStr != "" {
		if q, err := strconv.Atoi(qualityStr); err == nil && q > 0 && q <= 100 {
			quality = q
		}
	}

	if formatStr := c.Query("format"); formatStr != "" {
		format = formatStr
	}

	options := &types.StreamOptions{
		FPS:      fps,
		Quality:  quality,
		Format:   types.ImageFormat(format),
	}

	// Set up the screenshot engine in the stream manager
	s.streamManager.SetEngine(s.engine)

	s.logger.Info("Starting WebSocket stream session",
		zap.Int("window_id", windowID),
		zap.Int("fps", fps),
		zap.Int("quality", quality),
		zap.String("format", format),
		zap.String("client_ip", c.ClientIP()),
	)

	// Special handling: if windowID is 0, capture full desktop
	if windowID == 0 {
		s.logger.Info("Using desktop capture mode for window ID 0")
	}

	// Start streaming session
	session, err := s.streamManager.StartSession(uintptr(windowID), options)
	if err != nil {
		s.logger.Error("Stream session failed",
			zap.Int("window_id", windowID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Set the WebSocket connection
	session.Conn = conn

	// Send session started message
	err = conn.WriteJSON(map[string]interface{}{
		"type":       "session_started",
		"session_id": session.ID,
		"timestamp":  time.Now(),
	})
	if err != nil {
		s.logger.Error("Failed to send session started message", zap.Error(err))
		return
	}

	// Handle WebSocket messages in a goroutine
	go s.streamManager.HandleClientMessages(session)

	// Wait for session to complete
	<-session.Context.Done()

	s.logger.Info("WebSocket stream session ended",
		zap.Int("window_id", windowID),
		zap.String("client_ip", c.ClientIP()),
	)
}

// getStreamStatus returns the current streaming status
func (s *Server) getStreamStatus(c *gin.Context) {
	stats := s.streamManager.GetStats()
	c.JSON(http.StatusOK, gin.H{
		"active_sessions": stats.ActiveSessions,
		"total_sessions":  stats.TotalSessions,
		"total_frames":    stats.TotalFrames,
		"uptime":          stats.Uptime.String(),
		"max_sessions":    s.config.StreamMaxSessions,
	})
}

// ============================================================
// Standard MCP Protocol (Streamable HTTP transport)
// ============================================================

// handleMCPProtocol handles standard MCP protocol requests
func (s *Server) handleMCPProtocol(c *gin.Context) {
	var req types.MCPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		s.sendMCPError(c, nil, -32700, "Parse error", nil)
		return
	}

	s.logger.Debug("MCP protocol request",
		zap.String("method", req.Method),
		zap.Any("id", req.ID),
	)

	// Notifications (no id) get 202 Accepted with no body
	if req.ID == nil {
		c.Status(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		s.handleMCPProtocolInitialize(c, &req)
	case "ping":
		s.sendMCPResult(c, req.ID, map[string]interface{}{})
	case "tools/list":
		s.handleMCPProtocolToolsList(c, &req)
	case "tools/call":
		s.handleMCPProtocolToolsCall(c, &req)
	default:
		s.sendMCPError(c, req.ID, -32601, "Method not found", nil)
	}
}

func (s *Server) handleMCPProtocolInitialize(c *gin.Context, req *types.MCPRequest) {
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "windows-screenshot",
			"version": "1.0.0",
		},
	}
	s.sendMCPResult(c, req.ID, result)
}

func (s *Server) handleMCPProtocolToolsList(c *gin.Context, req *types.MCPRequest) {
	tools := []map[string]interface{}{
		{
			"name":        "take_screenshot",
			"description": "Capture a screenshot of a window identified by title, process ID, window handle, or class name. Returns the image as base64-encoded PNG/JPEG.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"method": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"title", "pid", "handle", "class"},
						"description": "How to identify the target window",
					},
					"target": map[string]interface{}{
						"type":        "string",
						"description": "Window identifier (exact title, PID number, handle number, or class name)",
					},
					"format": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"png", "jpeg"},
						"default":     "png",
						"description": "Output image format",
					},
				},
				"required": []string{"method", "target"},
			},
		},
		{
			"name":        "capture_desktop",
			"description": "Capture a full screenshot of the entire desktop / primary monitor.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			"name":        "list_windows",
			"description": "List all visible windows with their titles, class names, and process IDs.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			"name":        "mouse_click",
			"description": "Click the mouse at screen-absolute coordinates, or at coordinates relative to a window's client area. Optionally bring the window to the foreground first.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"x": map[string]interface{}{
						"type":        "integer",
						"description": "X coordinate in pixels",
					},
					"y": map[string]interface{}{
						"type":        "integer",
						"description": "Y coordinate in pixels",
					},
					"button": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"left", "right", "middle"},
						"default":     "left",
						"description": "Mouse button to click",
					},
					"click_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"single", "double"},
						"default":     "single",
						"description": "Single or double click",
					},
					"window_title": map[string]interface{}{
						"type":        "string",
						"description": "If provided, x/y are relative to this window's client area and the window is brought to the foreground first",
					},
				},
				"required": []string{"x", "y"},
			},
		},
		{
			"name":        "control_window",
			"description": "Control a window's state, position, or size. Actions: restore, maximize, minimize, focus, move, resize, move_resize.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"method": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"title", "pid", "handle", "class"},
						"description": "How to identify the target window",
					},
					"target": map[string]interface{}{
						"type":        "string",
						"description": "Window identifier (exact title, PID number, handle number, or class name)",
					},
					"action": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"restore", "maximize", "minimize", "focus", "move", "resize", "move_resize"},
						"description": "The action to perform on the window",
					},
					"x": map[string]interface{}{
						"type":        "integer",
						"description": "X position in pixels (for move/move_resize)",
					},
					"y": map[string]interface{}{
						"type":        "integer",
						"description": "Y position in pixels (for move/move_resize)",
					},
					"width": map[string]interface{}{
						"type":        "integer",
						"description": "Width in pixels (for resize/move_resize)",
					},
					"height": map[string]interface{}{
						"type":        "integer",
						"description": "Height in pixels (for resize/move_resize)",
					},
				},
				"required": []string{"method", "target", "action"},
			},
		},
	}

	result := map[string]interface{}{
		"tools": tools,
	}
	s.sendMCPResult(c, req.ID, result)
}

func (s *Server) handleMCPProtocolToolsCall(c *gin.Context, req *types.MCPRequest) {
	params, ok := req.Params.(map[string]interface{})
	if !ok {
		s.sendMCPError(c, req.ID, -32602, "Invalid params", nil)
		return
	}

	toolName := getString(params, "name", "")
	args, _ := params["arguments"].(map[string]interface{})
	if args == nil {
		args = map[string]interface{}{}
	}

	switch toolName {
	case "take_screenshot":
		s.mcpToolTakeScreenshot(c, req, args)
	case "capture_desktop":
		s.mcpToolCaptureDesktop(c, req)
	case "list_windows":
		s.mcpToolListWindows(c, req)
	case "control_window":
		s.mcpToolControlWindow(c, req, args)
	case "mouse_click":
		s.mcpToolMouseClick(c, req, args)
	default:
		s.sendMCPError(c, req.ID, -32602, fmt.Sprintf("Unknown tool: %s", toolName), nil)
	}
}

func (s *Server) mcpToolTakeScreenshot(c *gin.Context, req *types.MCPRequest, args map[string]interface{}) {
	method := getString(args, "method", "title")
	target := getString(args, "target", "")
	format := getString(args, "format", "png")

	if target == "" {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": "Missing required argument: target"},
			},
		})
		return
	}

	options := &types.CaptureOptions{
		IncludeCursor:    false,
		IncludeFrame:     true,
		ScaleFactor:      1.0,
		AllowMinimized:   true,
		RestoreWindow:    false,
		WaitForVisible:   2 * time.Second,
		RetryCount:       3,
		CustomProperties: make(map[string]string),
	}

	var buffer *types.ScreenshotBuffer
	var err error

	switch method {
	case "title":
		buffer, err = s.engine.CaptureByTitle(target, options)
	case "pid":
		if pid, parseErr := strconv.ParseUint(target, 10, 32); parseErr == nil {
			buffer, err = s.engine.CaptureByPID(uint32(pid), options)
		} else {
			err = fmt.Errorf("invalid PID: %s", target)
		}
	case "handle":
		if handle, parseErr := strconv.ParseUint(target, 10, 64); parseErr == nil {
			buffer, err = s.engine.CaptureByHandle(uintptr(handle), options)
		} else {
			err = fmt.Errorf("invalid handle: %s", target)
		}
	case "class":
		buffer, err = s.engine.CaptureByClassName(target, options)
	default:
		err = fmt.Errorf("unsupported method: %s", method)
	}

	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Screenshot failed: %s", err.Error())},
			},
		})
		return
	}

	// Validate captured dimensions — reject tiny/unusable captures
	const minUsableWidth = 200
	const minUsableHeight = 100
	if buffer.Width < minUsableWidth || buffer.Height < minUsableHeight {
		state := buffer.WindowInfo.State
		if state == "" {
			state = "unknown"
		}
		msg := fmt.Sprintf(
			"Window is too small to capture usefully (%dx%d, state: %s). "+
				"The window may be minimized or resized to its title bar. "+
				"Please restore or resize the window and try again.",
			buffer.Width, buffer.Height, state)
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": msg},
			},
		})
		return
	}

	imageFormat := types.FormatPNG
	mimeType := "image/png"
	if format == "jpeg" {
		imageFormat = types.FormatJPEG
		mimeType = "image/jpeg"
	}

	encoded, err := s.processor.Encode(buffer, imageFormat, s.config.Quality)
	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Image encoding failed: %s", err.Error())},
			},
		})
		return
	}

	imageData := base64.StdEncoding.EncodeToString(encoded)

	s.sendMCPResult(c, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":     "image",
				"data":     imageData,
				"mimeType": mimeType,
			},
			{
				"type": "text",
				"text": fmt.Sprintf("Screenshot captured: %dx%d %s (%d bytes)",
					buffer.Width, buffer.Height, format, len(encoded)),
			},
		},
	})
}

func (s *Server) mcpToolCaptureDesktop(c *gin.Context, req *types.MCPRequest) {
	options := &types.CaptureOptions{
		IncludeCursor:    true,
		IncludeFrame:     true,
		ScaleFactor:      1.0,
		CustomProperties: make(map[string]string),
	}

	buffer, err := s.engine.CaptureFullScreen(0, options)
	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Desktop capture failed: %s", err.Error())},
			},
		})
		return
	}

	encoded, err := s.processor.Encode(buffer, types.FormatPNG, s.config.Quality)
	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Image encoding failed: %s", err.Error())},
			},
		})
		return
	}

	imageData := base64.StdEncoding.EncodeToString(encoded)

	s.sendMCPResult(c, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type":     "image",
				"data":     imageData,
				"mimeType": "image/png",
			},
			{
				"type": "text",
				"text": fmt.Sprintf("Desktop captured: %dx%d (%d bytes)",
					buffer.Width, buffer.Height, len(encoded)),
			},
		},
	})
}

func (s *Server) mcpToolListWindows(c *gin.Context, req *types.MCPRequest) {
	windows, err := s.engine.ListVisibleWindows()
	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Failed to list windows: %s", err.Error())},
			},
		})
		return
	}

	text := fmt.Sprintf("Found %d visible windows:\n\n", len(windows))
	for _, w := range windows {
		text += fmt.Sprintf("- \"%s\" (class: %s, PID: %d, handle: %d, size: %dx%d, state: %s)\n",
			w.Title, w.ClassName, w.ProcessID, w.Handle,
			w.Rect.Width, w.Rect.Height, w.State)
	}

	s.sendMCPResult(c, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": text},
		},
	})
}

func (s *Server) mcpToolControlWindow(c *gin.Context, req *types.MCPRequest, args map[string]interface{}) {
	method := getString(args, "method", "title")
	target := getString(args, "target", "")
	action := getString(args, "action", "")

	if target == "" || action == "" {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": "Missing required arguments: target and action"},
			},
		})
		return
	}

	// Resolve window handle
	var handle uintptr
	var err error

	switch method {
	case "title":
		handle, err = s.engine.FindWindowHandle("title", target)
	case "pid":
		if pid, parseErr := strconv.ParseUint(target, 10, 32); parseErr == nil {
			handle, err = s.engine.FindWindowByPIDPublic(uint32(pid))
		} else {
			err = fmt.Errorf("invalid PID: %s", target)
		}
	case "handle":
		if h, parseErr := strconv.ParseUint(target, 10, 64); parseErr == nil {
			handle = uintptr(h)
		} else {
			err = fmt.Errorf("invalid handle: %s", target)
		}
	case "class":
		handle, err = s.engine.FindWindowHandle("class", target)
	default:
		err = fmt.Errorf("unsupported method: %s", method)
	}

	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Window not found: %s", err.Error())},
			},
		})
		return
	}

	// Parse optional position/size args
	x := getInt(args, "x", 0)
	y := getInt(args, "y", 0)
	width := getInt(args, "width", 800)
	height := getInt(args, "height", 600)

	info, err := s.engine.ControlWindow(handle, action, x, y, width, height)
	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Control failed: %s", err.Error())},
			},
		})
		return
	}

	s.sendMCPResult(c, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": fmt.Sprintf("Window '%s' %s done. New state: %s, size: %dx%d, position: (%d,%d)",
					info.Title, action, info.State,
					info.Rect.Width, info.Rect.Height,
					info.Rect.X, info.Rect.Y),
			},
		},
	})
}

func (s *Server) mcpToolMouseClick(c *gin.Context, req *types.MCPRequest, args map[string]interface{}) {
	x := getInt(args, "x", 0)
	y := getInt(args, "y", 0)
	button := getString(args, "button", "left")
	clickType := getString(args, "click_type", "single")
	windowTitle := getString(args, "window_title", "")

	var windowHandle uintptr
	if windowTitle != "" {
		handle, err := s.engine.FindWindowHandle("title", windowTitle)
		if err != nil {
			s.sendMCPResult(c, req.ID, map[string]interface{}{
				"isError": true,
				"content": []map[string]interface{}{
					{"type": "text", "text": fmt.Sprintf("Window not found: %s", err.Error())},
				},
			})
			return
		}
		windowHandle = handle
	}

	err := s.engine.ClickMouse(x, y, button, clickType, windowHandle)
	if err != nil {
		s.sendMCPResult(c, req.ID, map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{"type": "text", "text": fmt.Sprintf("Mouse click failed: %s", err.Error())},
			},
		})
		return
	}

	coordDesc := fmt.Sprintf("screen (%d,%d)", x, y)
	mode := "physical"
	if windowTitle != "" {
		coordDesc = fmt.Sprintf("window '%s' client (%d,%d)", windowTitle, x, y)
		mode = "stealth (PostMessage)"
	}

	s.sendMCPResult(c, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": fmt.Sprintf("%s %s-click at %s [%s]",
					button, clickType, coordDesc, mode),
			},
		},
	})
}

// main function
func main() {
	server, err := NewServer()
	if err != nil {
		log.Fatal("Failed to create server:", err)
	}

	if err := server.Start(); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}
