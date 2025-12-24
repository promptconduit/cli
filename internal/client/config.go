package client

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	ConfigDirName        = ".promptconduit"
	ConfigFileName       = "config.json"
)

// Config holds the client configuration
type Config struct {
	APIKey         string `json:"api_key,omitempty"`
	APIURL         string `json:"api_url,omitempty"`
	Debug          bool   `json:"debug,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// FileConfig represents the config file structure with environment support
type FileConfig struct {
	// Current environment name (local, dev, prod, or custom)
	CurrentEnv string `json:"current_env,omitempty"`

	// Environment-specific configurations
	Environments map[string]*Config `json:"environments,omitempty"`

	// Legacy flat config (for backwards compatibility)
	APIKey  string `json:"api_key,omitempty"`
	APIURL  string `json:"api_url,omitempty"`
	Debug   bool   `json:"debug,omitempty"`
	Timeout int    `json:"timeout_seconds,omitempty"`
}

// IsConfigured returns true if the API key is set
func (c *Config) IsConfigured() bool {
	return c.APIKey != ""
}

// ConfigPath returns the path to the config file
func ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ConfigDirName, ConfigFileName)
}

// ConfigDir returns the path to the config directory
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ConfigDirName)
}

// LoadFileConfig loads the config file from disk
func LoadFileConfig() (*FileConfig, error) {
	path := ConfigPath()
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var fc FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, err
	}

	return &fc, nil
}

// SaveFileConfig saves the config to disk
func SaveFileConfig(fc *FileConfig) error {
	dir := ConfigDir()
	if dir == "" {
		return os.ErrNotExist
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ConfigPath(), data, 0600)
}

// GetCurrentConfig returns the active config from the file (based on current_env)
func (fc *FileConfig) GetCurrentConfig() *Config {
	if fc == nil {
		return nil
	}

	// If we have environments and a current env, use that
	if fc.CurrentEnv != "" && fc.Environments != nil {
		if cfg, ok := fc.Environments[fc.CurrentEnv]; ok {
			return cfg
		}
	}

	// Fall back to legacy flat config
	if fc.APIKey != "" || fc.APIURL != "" {
		return &Config{
			APIKey:         fc.APIKey,
			APIURL:         fc.APIURL,
			Debug:          fc.Debug,
			TimeoutSeconds: fc.Timeout,
		}
	}

	return nil
}

// LoadConfig loads configuration from environment variables, falling back to config file
func LoadConfig() *Config {
	cfg := &Config{
		APIKey:         os.Getenv(EnvAPIKey),
		APIURL:         os.Getenv(EnvAPIURL),
		Debug:          os.Getenv(EnvDebug) == "1" || os.Getenv(EnvDebug) == "true",
		TimeoutSeconds: DefaultTimeoutSecs,
	}

	// If API key not set via env, try config file
	if cfg.APIKey == "" {
		if fc, err := LoadFileConfig(); err == nil && fc != nil {
			if fileCfg := fc.GetCurrentConfig(); fileCfg != nil {
				if cfg.APIKey == "" {
					cfg.APIKey = fileCfg.APIKey
				}
				if cfg.APIURL == "" && fileCfg.APIURL != "" {
					cfg.APIURL = fileCfg.APIURL
				}
				if !cfg.Debug && fileCfg.Debug {
					cfg.Debug = true
				}
				if fileCfg.TimeoutSeconds > 0 {
					cfg.TimeoutSeconds = fileCfg.TimeoutSeconds
				}
			}
		}
	}

	// Apply defaults
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}

	// Check env timeout override
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
