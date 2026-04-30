package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/n9e/n9e-mcp-server/pkg/api"
	"github.com/n9e/n9e-mcp-server/pkg/client"
	"github.com/n9e/n9e-mcp-server/pkg/toolset"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Header names for Nightingale API when passed from MCP client (e.g. Cursor).
// When set, they override the server's startup N9E_TOKEN / N9E_BASE_URL for that session.
const (
	N9eTokenHeader  = "X-User-Token"
	N9eBaseURLHeader = "X-N9e-Base-Url"
)

// ServerConfig represents MCP Server configuration
type ServerConfig struct {
	Version         string
	Token           string
	BaseURL         string
	EnabledToolsets []string
	ReadOnly        bool
}

// NewMCPServer creates MCP Server
func NewMCPServer(cfg ServerConfig) (*mcp.Server, error) {
	// Create N9e Client
	n9eClient, err := client.NewClient(cfg.Token, cfg.BaseURL, fmt.Sprintf("n9e-mcp-server/%s", cfg.Version))
	if err != nil {
		return nil, fmt.Errorf("failed to create n9e client: %w", err)
	}

	// Create MCP Server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "n9e-mcp-server",
		Version: cfg.Version,
	}, &mcp.ServerOptions{
		Instructions: "Nightingale (n9e) monitoring MCP Server. Provides alert rule management, " +
			"active/history alert querying, alert mute/silence management, notification rules, " +
			"alert subscriptions, user/team management, monitored target management, " +
			"datasource management, business group management, and event pipeline/workflow management.",
		Logger: slog.Default(),
	})

	// Add middleware: inject client into context
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx = client.ContextWithClient(ctx, n9eClient)
			return next(ctx, method, req)
		}
	})

	// Create toolset group
	getClient := func(ctx context.Context) *client.Client {
		return client.ClientFromContext(ctx)
	}
	toolsetGroup := api.DefaultToolsetGroup(getClient, cfg.ReadOnly)

	// Determine enabled toolsets
	enabledToolsets := cfg.EnabledToolsets
	if len(enabledToolsets) == 0 {
		enabledToolsets = toolset.DefaultToolsets
	}

	// Enable toolsets
	if err := toolsetGroup.EnableToolsets(enabledToolsets); err != nil {
		return nil, fmt.Errorf("failed to enable toolsets: %w", err)
	}

	// Register all tools
	toolsetGroup.RegisterAll(server)

	return server, nil
}

// StdioServerConfig represents stdio mode configuration
type StdioServerConfig struct {
	Version         string
	Token           string
	BaseURL         string
	EnabledToolsets []string
	ReadOnly        bool
	LogFilePath     string
}

// RunStdioServer runs stdio mode server
func RunStdioServer(cfg StdioServerConfig) error {
	// Create context with signal interrupt support
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Configure logging (with file rotation support)
	var logOutput io.Writer = os.Stderr
	if cfg.LogFilePath != "" {
		logOutput = &lumberjack.Logger{
			Filename:   cfg.LogFilePath,
			MaxSize:    100, // MB, max file size
			MaxBackups: 3,   // Number of old files to keep
			MaxAge:     7,   // Days to keep
			Compress:   true,
		}
	}

	// Use LevelVar to support dynamic log level modification at runtime
	var logLevel slog.LevelVar
	parseLogLevel := func() slog.Level {
		switch os.Getenv("N9E_MCP_LOG_LEVEL") {
		case "debug", "DEBUG":
			return slog.LevelDebug
		case "warn", "WARN":
			return slog.LevelWarn
		case "error", "ERROR":
			return slog.LevelError
		default:
			return slog.LevelInfo
		}
	}
	logLevel.Set(parseLogLevel())
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: &logLevel}))
	slog.SetDefault(logger)

	// Listen to SIGUSR1 signal to reload environment variables and update log level (Unix only)
	setupSignalReload(func() {
		newLevel := parseLogLevel()
		logLevel.Set(newLevel)
		logger.Info("log level reloaded", "level", newLevel.String())
	})

	logger.Info("starting n9e-mcp-server",
		"version", cfg.Version,
		"base_url", cfg.BaseURL,
		"read_only", cfg.ReadOnly,
		"toolsets", cfg.EnabledToolsets,
	)

	// Create MCP Server
	server, err := NewMCPServer(ServerConfig{
		Version:         cfg.Version,
		Token:           cfg.Token,
		BaseURL:         cfg.BaseURL,
		EnabledToolsets: cfg.EnabledToolsets,
		ReadOnly:        cfg.ReadOnly,
	})
	if err != nil {
		return fmt.Errorf("failed to create MCP server: %w", err)
	}

	// Run server
	errC := make(chan error, 1)
	go func() {
		errC <- server.Run(ctx, &mcp.StdioTransport{})
	}()

	fmt.Fprintln(os.Stderr, "Nightingale MCP Server running on stdio")

	// Wait for exit
	select {
	case <-ctx.Done():
		logger.Info("shutting down server...")
	case err := <-errC:
		if err != nil {
			logger.Error("server error", "error", err)
			return fmt.Errorf("server error: %w", err)
		}
	}

	return nil
}

// HTTPServerConfig represents HTTP mode configuration.
type HTTPServerConfig struct {
	Version         string
	Token           string
	BaseURL         string
	EnabledToolsets []string
	ReadOnly        bool
	LogFilePath     string
	ListenAddr      string        // e.g. ":8080" or "0.0.0.0:8080"
	SessionTimeout  time.Duration // zero means no idle timeout
	// Shared: when true, server must be started with N9E_TOKEN and N9E_BASE_URL; client headers (X-User-Token, X-N9e-Base-Url) are ignored.
	// When false, token and base URL may be omitted at startup; each request must provide them via headers (mcp.json).
	Shared bool
}

// RunHTTPServer runs the MCP server over HTTP using the streamable transport.
// It uses JSON request/response only (no SSE). Each request is handled via
// standard HTTP POST; session state is maintained via Mcp-Session-Id header.
func RunHTTPServer(cfg HTTPServerConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var logOutput io.Writer = os.Stderr
	if cfg.LogFilePath != "" {
		logOutput = &lumberjack.Logger{
			Filename:   cfg.LogFilePath,
			MaxSize:    100,
			MaxBackups: 3,
			MaxAge:     7,
			Compress:   true,
		}
	}

	var logLevel slog.LevelVar
	parseLogLevel := func() slog.Level {
		switch os.Getenv("N9E_MCP_LOG_LEVEL") {
		case "debug", "DEBUG":
			return slog.LevelDebug
		case "warn", "WARN":
			return slog.LevelWarn
		case "error", "ERROR":
			return slog.LevelError
		default:
			return slog.LevelInfo
		}
	}
	logLevel.Set(parseLogLevel())
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: &logLevel}))
	slog.SetDefault(logger)

	setupSignalReload(func() {
		newLevel := parseLogLevel()
		logLevel.Set(newLevel)
		logger.Info("log level reloaded", "level", newLevel.String())
	})

	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	// shared=true: require token and base URL at startup; client headers are ignored.
	if cfg.Shared {
		if cfg.Token == "" || cfg.BaseURL == "" {
			return fmt.Errorf("when shared=true (HTTP), N9E_TOKEN and N9E_BASE_URL are required at startup")
		}
	}

	logger.Info("starting n9e-mcp-server (HTTP)",
		"version", cfg.Version,
		"listen", listenAddr,
		"shared", cfg.Shared,
		"base_url", cfg.BaseURL,
		"read_only", cfg.ReadOnly,
		"toolsets", cfg.EnabledToolsets,
	)

	var getServerForRequest func(*http.Request) *mcp.Server

	if cfg.Shared {
		// Shared mode: single server from startup config; ignore X-User-Token and X-N9e-Base-Url.
		defaultServer, err := NewMCPServer(ServerConfig{
			Version:         cfg.Version,
			Token:           cfg.Token,
			BaseURL:         cfg.BaseURL,
			EnabledToolsets: cfg.EnabledToolsets,
			ReadOnly:        cfg.ReadOnly,
		})
		if err != nil {
			return fmt.Errorf("failed to create MCP server: %w", err)
		}
		getServerForRequest = func(*http.Request) *mcp.Server { return defaultServer }
	} else {
		// Non-shared: token and base URL may be omitted at startup; each request must provide X-User-Token and X-N9e-Base-Url.
		var defaultServer *mcp.Server
		if cfg.Token != "" && cfg.BaseURL != "" {
			var err error
			defaultServer, err = NewMCPServer(ServerConfig{
				Version:         cfg.Version,
				Token:           cfg.Token,
				BaseURL:         cfg.BaseURL,
				EnabledToolsets: cfg.EnabledToolsets,
				ReadOnly:        cfg.ReadOnly,
			})
			if err != nil {
				return fmt.Errorf("failed to create default MCP server: %w", err)
			}
		}
		var serverCache sync.Map // "token|baseURL" -> *mcp.Server
		getServerForRequest = func(req *http.Request) *mcp.Server {
			token := strings.TrimSpace(req.Header.Get(N9eTokenHeader))
			baseURL := strings.TrimSpace(req.Header.Get(N9eBaseURLHeader))
			if baseURL != "" {
				if _, err := url.Parse(baseURL); err != nil {
					logger.Warn("invalid X-N9e-Base-Url header", "value", baseURL, "error", err)
					baseURL = ""
				}
			}
			// No headers: use default server if startup provided token+baseURL; otherwise require headers.
			if token == "" && baseURL == "" {
				if defaultServer != nil {
					return defaultServer
				}
				logger.Warn("request missing X-User-Token and X-N9e-Base-Url (shared=false, no startup config)")
				return nil
			}
			if token == "" {
				token = cfg.Token
			}
			if baseURL == "" {
				baseURL = cfg.BaseURL
			}
			if token == "" || baseURL == "" {
				logger.Warn("request missing X-User-Token or X-N9e-Base-Url (shared=false)")
				return nil
			}
			cacheKey := token + "|" + baseURL
			if s, ok := serverCache.Load(cacheKey); ok {
				return s.(*mcp.Server)
			}
			s, err := NewMCPServer(ServerConfig{
				Version:         cfg.Version,
				Token:           token,
				BaseURL:         baseURL,
				EnabledToolsets: cfg.EnabledToolsets,
				ReadOnly:        cfg.ReadOnly,
			})
			if err != nil {
				logger.Warn("failed to create MCP server for request headers", "error", err)
				if defaultServer != nil {
					return defaultServer
				}
				return nil
			}
			serverCache.Store(cacheKey, s)
			return s
		}
	}

	// Streamable HTTP handler with JSON response only (no SSE).
	opts := &mcp.StreamableHTTPOptions{
		JSONResponse:   true,
		Logger:         logger,
		SessionTimeout: cfg.SessionTimeout,
	}
	handler := mcp.NewStreamableHTTPHandler(getServerForRequest, opts)

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errC := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errC <- err
		} else {
			errC <- nil
		}
	}()

	logger.Info("Nightingale MCP Server (HTTP) listening", "addr", listenAddr)
	fmt.Fprintf(os.Stderr, "Nightingale MCP Server (HTTP) listening on %s\n", listenAddr)

	select {
	case <-ctx.Done():
		logger.Info("shutting down HTTP server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", "error", err)
		}
	case err := <-errC:
		if err != nil {
			logger.Error("server error", "error", err)
			return fmt.Errorf("server error: %w", err)
		}
	}

	return nil
}
