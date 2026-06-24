package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

const (
	DefaultAPIURL      = "https://api.promptconduit.dev"
	DefaultTimeoutSecs = 30
	EnvAPIKey          = "PROMPTCONDUIT_API_KEY"
	EnvAPIURL          = "PROMPTCONDUIT_API_URL"
	EnvDebug           = "PROMPTCONDUIT_DEBUG"
	EnvTimeout         = "PROMPTCONDUIT_TIMEOUT"
	EnvTool            = "PROMPTCONDUIT_TOOL"
	EnvAutoUpdate      = "PROMPTCONDUIT_AUTO_UPDATE" // "0"/"false" disables background self-upgrade
	EnvEventLog        = "PROMPTCONDUIT_EVENT_LOG"   // "0"/"false" disables the local ~/.promptconduit event log
	EnvLocalOnly       = "PROMPTCONDUIT_LOCAL_ONLY"  // "1"/"true" forces Free / local-only mode (never send to the cloud)
	EnvXDGConfigHome   = "XDG_CONFIG_HOME"
	ConfigDirName      = "promptconduit" // ~/.config/promptconduit/
	ConfigFileName     = "config.json"
)

// Config holds the client configuration
type Config struct {
	APIKey         string `json:"api_key,omitempty"`
	APIURL         string `json:"api_url,omitempty"`
	Debug          bool   `json:"debug,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	// DisableAutoUpdate opts out of the background "check + self-upgrade"
	// behaviour. Zero value (false) leaves auto-update enabled.
	DisableAutoUpdate bool `json:"disable_auto_update,omitempty"`
	// DisableEventLog opts out of the local full-fidelity event log written
	// to ~/.promptconduit/ (events.ndjson, errors.log, status.json). Zero
	// value (false) leaves the event log enabled — it's on by default.
	DisableEventLog bool `json:"disable_event_log,omitempty"`
	// LocalOnly is the Free tier: events are captured to the local event log
	// but NEVER sent to the cloud, regardless of whether an API key is set.
	// Zero value (false) leaves cloud sync enabled (when an API key exists).
	LocalOnly bool `json:"local_only,omitempty"`
}

// FileConfig represents the config file structure with environment support
type FileConfig struct {
	// Current environment name (local, dev, prod, or custom)
	CurrentEnv string `json:"current_env,omitempty"`

	// Environment-specific configurations
	Environments map[string]*Config `json:"environments,omitempty"`

	// Legacy flat config (for backwards compatibility)
	APIKey            string `json:"api_key,omitempty"`
	APIURL            string `json:"api_url,omitempty"`
	Debug             bool   `json:"debug,omitempty"`
	Timeout           int    `json:"timeout_seconds,omitempty"`
	DisableAutoUpdate bool   `json:"disable_auto_update,omitempty"`
	DisableEventLog   bool   `json:"disable_event_log,omitempty"`
	LocalOnly         bool   `json:"local_only,omitempty"`
}

// EventLogEnabled reports whether the local ~/.promptconduit event log should
// be written. On by default; disabled only when explicitly opted out via
// config (disable_event_log) or PROMPTCONDUIT_EVENT_LOG=0.
func (c *Config) EventLogEnabled() bool {
	return !c.DisableEventLog
}

// IsConfigured returns true if the API key is set
func (c *Config) IsConfigured() bool {
	return c.APIKey != ""
}

// ShouldSend reports whether captured events should be sent to the cloud. It is
// the single send gate: events go to the platform only when an API key is set
// AND local-only mode is off. A missing API key OR LocalOnly both mean Free /
// local-only — events are still captured locally, just never transmitted.
func (c *Config) ShouldSend() bool {
	return c.IsConfigured() && !c.LocalOnly
}

// ConfigPath returns the path to the config file (XDG standard)
// Uses $XDG_CONFIG_HOME/promptconduit/config.json if set, otherwise ~/.config/promptconduit/config.json
func ConfigPath() string {
	if xdgConfig := os.Getenv(EnvXDGConfigHome); xdgConfig != "" {
		return filepath.Join(xdgConfig, ConfigDirName, ConfigFileName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", ConfigDirName, ConfigFileName)
}

// ConfigDir returns the path to the config directory
func ConfigDir() string {
	path := ConfigPath()
	if path == "" {
		return ""
	}
	return filepath.Dir(path)
}

// AllConfigPaths returns all possible config paths for display/debugging
func AllConfigPaths() []string {
	path := ConfigPath()
	if path == "" {
		return nil
	}
	return []string{path}
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
	if fc.APIKey != "" || fc.APIURL != "" || fc.DisableAutoUpdate || fc.DisableEventLog || fc.LocalOnly {
		return &Config{
			APIKey:            fc.APIKey,
			APIURL:            fc.APIURL,
			Debug:             fc.Debug,
			TimeoutSeconds:    fc.Timeout,
			DisableAutoUpdate: fc.DisableAutoUpdate,
			DisableEventLog:   fc.DisableEventLog,
			LocalOnly:         fc.LocalOnly,
		}
	}

	return nil
}

// LoadConfig loads configuration from environment variables and config file
// Environment variables take precedence over file config
func LoadConfig() *Config {
	cfg := &Config{
		APIKey:         os.Getenv(EnvAPIKey),
		APIURL:         os.Getenv(EnvAPIURL),
		Debug:          os.Getenv(EnvDebug) == "1" || os.Getenv(EnvDebug) == "true",
		TimeoutSeconds: DefaultTimeoutSecs,
	}

	// Always load file config and merge (env vars take precedence)
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
			if fileCfg.DisableAutoUpdate {
				cfg.DisableAutoUpdate = true
			}
			if fileCfg.DisableEventLog {
				cfg.DisableEventLog = true
			}
			if fileCfg.LocalOnly {
				cfg.LocalOnly = true
			}
		}
	}

	// PROMPTCONDUIT_AUTO_UPDATE=0/false disables; anything else (or unset)
	// leaves whatever the file config decided.
	if v := os.Getenv(EnvAutoUpdate); v == "0" || v == "false" || v == "no" {
		cfg.DisableAutoUpdate = true
	}

	// PROMPTCONDUIT_EVENT_LOG=0/false/no disables the local event log;
	// anything else (or unset) leaves whatever the file config decided.
	if v := os.Getenv(EnvEventLog); v == "0" || v == "false" || v == "no" {
		cfg.DisableEventLog = true
	}

	// PROMPTCONDUIT_LOCAL_ONLY=1/true/yes forces Free / local-only mode;
	// anything else (or unset) leaves whatever the file config decided.
	if v := os.Getenv(EnvLocalOnly); v == "1" || v == "true" || v == "yes" {
		cfg.LocalOnly = true
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
