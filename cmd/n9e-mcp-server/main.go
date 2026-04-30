package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/n9e/n9e-mcp-server/internal"
	"github.com/n9e/n9e-mcp-server/pkg/toolset"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "n9e-mcp-server",
	Short: "Nightingale MCP Server",
	Long:  "MCP (Model Context Protocol) server for Nightingale",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Default to run in stdio mode
		return runStdio(cmd, args)
	},
}

var stdioCmd = &cobra.Command{
	Use:   "stdio",
	Short: "Run in stdio mode (default)",
	Long:  "Run the MCP server using stdin/stdout for communication",
	RunE:  runStdio,
}

var httpCmd = &cobra.Command{
	Use:   "http",
	Short: "Run in HTTP mode (streamable transport, JSON only, no SSE)",
	Long:  "Run the MCP server over HTTP. Uses MCP streamable transport with application/json request/response (no server-sent events).",
	RunE:  runHTTP,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("n9e-mcp-server %s (commit: %s, built: %s)\n", version, commit, date)
	},
}

func init() {
	// Environment variable prefix
	viper.SetEnvPrefix("N9E")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	// Global flags
	rootCmd.PersistentFlags().String("token", "", "Nightingale API token (env: N9E_TOKEN)")
	rootCmd.PersistentFlags().String("base-url", "http://localhost:17000", "Nightingale API base URL (env: N9E_BASE_URL)")
	rootCmd.PersistentFlags().StringSlice("toolsets", toolset.DefaultToolsets, "Enabled toolsets (env: N9E_TOOLSETS)")
	rootCmd.PersistentFlags().Bool("read-only", false, "Read-only mode, disable write operations (env: N9E_READ_ONLY)")
	rootCmd.PersistentFlags().String("log-file", "", "Log file path (default: stderr)")

	// Bind to viper
	viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
	viper.BindPFlag("base_url", rootCmd.PersistentFlags().Lookup("base-url"))
	viper.BindPFlag("toolsets", rootCmd.PersistentFlags().Lookup("toolsets"))
	viper.BindPFlag("read_only", rootCmd.PersistentFlags().Lookup("read-only"))
	viper.BindPFlag("log_file", rootCmd.PersistentFlags().Lookup("log-file"))

	// HTTP-specific flags
	httpCmd.Flags().String("listen", ":8080", "Listen address (env: N9E_LISTEN)")
	httpCmd.Flags().Duration("session-timeout", 0, "Idle session timeout (0 = no timeout, env: N9E_SESSION_TIMEOUT)")
	httpCmd.Flags().Bool("shared", false, "Shared mode: require N9E_TOKEN and N9E_BASE_URL at startup, ignore client headers (env: N9E_SHARED)")
	viper.BindPFlag("listen", httpCmd.Flags().Lookup("listen"))
	viper.BindPFlag("session_timeout", httpCmd.Flags().Lookup("session-timeout"))
	viper.BindPFlag("shared", httpCmd.Flags().Lookup("shared"))

	// Add subcommands
	rootCmd.AddCommand(stdioCmd)
	rootCmd.AddCommand(httpCmd)
	rootCmd.AddCommand(versionCmd)
}

func runStdio(cmd *cobra.Command, args []string) error {
	token := viper.GetString("token")
	if token == "" {
		return fmt.Errorf("N9E_TOKEN is required. Set it via --token flag or N9E_TOKEN environment variable")
	}

	return internal.RunStdioServer(internal.StdioServerConfig{
		Version:         version,
		Token:           token,
		BaseURL:         viper.GetString("base_url"),
		EnabledToolsets: viper.GetStringSlice("toolsets"),
		ReadOnly:        viper.GetBool("read_only"),
		LogFilePath:     viper.GetString("log_file"),
	})
}

func runHTTP(cmd *cobra.Command, args []string) error {
	shared := viper.GetBool("shared")
	token := viper.GetString("token")
	baseURL := viper.GetString("base_url")

	if shared {
		if token == "" || baseURL == "" {
			return fmt.Errorf("when --shared is true, N9E_TOKEN and N9E_BASE_URL are required")
		}
	}

	return internal.RunHTTPServer(internal.HTTPServerConfig{
		Version:         version,
		Token:           token,
		BaseURL:         baseURL,
		EnabledToolsets: viper.GetStringSlice("toolsets"),
		ReadOnly:        viper.GetBool("read_only"),
		LogFilePath:     viper.GetString("log_file"),
		ListenAddr:      viper.GetString("listen"),
		SessionTimeout:  viper.GetDuration("session_timeout"),
		Shared:          shared,
	})
}
