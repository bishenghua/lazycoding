package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"lazycoding/internal/transcribe"
)

// Config is the root configuration structure.
type Config struct {
	Telegram      TelegramConfig            `yaml:"telegram"`
	Claude        ClaudeConfig              `yaml:"claude"`
	Channels      map[string]*ChannelConfig `yaml:"channels"` // key = Telegram chat ID (string)
	Transcription TranscriptionConfig       `yaml:"transcription"`
	Log           LogConfig                 `yaml:"log"`
}

// TelegramConfig holds Telegram-specific settings.
type TelegramConfig struct {
	Token          string  `yaml:"token"`
	AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
	EditThrottleMs int     `yaml:"edit_throttle_ms"`
}

// ClaudeConfig holds global defaults for the Claude CLI runner.
type ClaudeConfig struct {
	WorkDir    string   `yaml:"work_dir"`
	ExtraFlags []string `yaml:"extra_flags"`
	TimeoutSec int      `yaml:"timeout_sec"`
}

// ChannelConfig overrides Claude settings for a specific Telegram chat.
// Any zero/nil field falls back to the global ClaudeConfig default.
type ChannelConfig struct {
	WorkDir    string   `yaml:"work_dir"`
	ExtraFlags []string `yaml:"extra_flags"` // nil = inherit global extra_flags
}

// TranscriptionConfig is an alias for the transcribe package's Config so that
// the YAML tags are defined in one place and config.Config can reference it
// without a circular import.
type TranscriptionConfig = transcribe.Config

// LogConfig controls structured logging.
type LogConfig struct {
	Format  string `yaml:"format"`  // "json" | "text"
	Level   string `yaml:"level"`   // "debug" | "info" | "warn" | "error"
	Verbose bool   `yaml:"verbose"` // print human-readable conversation transcript to stderr
}

// EditThrottle returns the configured throttle duration for in-place edits.
func (c *TelegramConfig) EditThrottle() time.Duration {
	if c.EditThrottleMs <= 0 {
		return 2 * time.Second
	}
	return time.Duration(c.EditThrottleMs) * time.Millisecond
}

// AllowedSet builds a fast-lookup set from AllowedUserIDs.
// An empty set means everyone is allowed.
func (c *Config) AllowedSet() map[int64]bool {
	set := make(map[int64]bool, len(c.Telegram.AllowedUserIDs))
	for _, id := range c.Telegram.AllowedUserIDs {
		set[id] = true
	}
	return set
}

// WorkDirFor returns the effective work_dir for the given conversation ID.
// Channel-specific override → global default → empty string (lazycoding launch dir).
func (c *Config) WorkDirFor(conversationID string) string {
	if ch, ok := c.Channels[conversationID]; ok && ch.WorkDir != "" {
		return ch.WorkDir
	}
	return c.Claude.WorkDir
}

// ExtraFlagsFor returns the effective extra_flags for the given conversation ID.
// A non-nil (even empty) slice in the channel override takes precedence over
// the global default, allowing per-channel suppression of global flags.
func (c *Config) ExtraFlagsFor(conversationID string) []string {
	if ch, ok := c.Channels[conversationID]; ok && ch.ExtraFlags != nil {
		return ch.ExtraFlags
	}
	return c.Claude.ExtraFlags
}

// Load reads and validates the YAML config at path, applying defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults.
	if cfg.Telegram.EditThrottleMs <= 0 {
		cfg.Telegram.EditThrottleMs = 1000
	}
	if cfg.Claude.TimeoutSec <= 0 {
		cfg.Claude.TimeoutSec = 300
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "text"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Channels == nil {
		cfg.Channels = make(map[string]*ChannelConfig)
	}
	// Defaults for whisper-cpp sub-config.
	if cfg.Transcription.WhisperCPP.Bin == "" {
		cfg.Transcription.WhisperCPP.Bin = "whisper-cli"
	}
	// Groq model default applied in transcribe.New().

	return &cfg, nil
}
