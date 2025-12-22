package client

import (
	"os"
	"strconv"
)

const (
	DefaultAPIURL        = "https://api.promptconduit.dev"
	DefaultTimeoutSecs   = 30
	EnvAPIKey            = "PROMPTCONDUIT_API_KEY"
	EnvAPIURL            = "PROMPTCONDUIT_API_URL"
	EnvDebug             = "PROMPTCONDUIT_DEBUG"
	EnvTimeout           = "PROMPTCONDUIT_TIMEOUT"
	EnvTool              = "PROMPTCONDUIT_TOOL"
)

// Config holds the client configuration
type Config struct {
	APIKey         string
	APIURL         string
	Debug          bool
	TimeoutSeconds int
}

// IsConfigured returns true if the API key is set
func (c *Config) IsConfigured() bool {
	return c.APIKey != ""
}

// LoadConfig loads configuration from environment variables
func LoadConfig() *Config {
	cfg := &Config{
		APIKey:         os.Getenv(EnvAPIKey),
		APIURL:         os.Getenv(EnvAPIURL),
		Debug:          os.Getenv(EnvDebug) == "1" || os.Getenv(EnvDebug) == "true",
		TimeoutSeconds: DefaultTimeoutSecs,
	}

	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}

	if timeoutStr := os.Getenv(EnvTimeout); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil && timeout > 0 {
			cfg.TimeoutSeconds = timeout
		}
	}

	return cfg
}

// MaskAPIKey returns a masked version of the API key for display
func MaskAPIKey(key string) string {
	if len(key) <= 4 {
		return "***"
	}
	return "***..." + key[len(key)-4:]
}
