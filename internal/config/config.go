package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bishenghua/lazycoding/internal/transcribe"
)

// Config is the root configuration structure.
type Config struct {
	Telegram      TelegramConfig            `yaml:"telegram"`
	Feishu        FeishuConfig              `yaml:"feishu"`
	QQBot         QQBotConfig               `yaml:"qqbot"`
	DingTalk      DingTalkConfig            `yaml:"dingtalk"`
	WeWork        WeWorkConfig              `yaml:"wework"`
	Claude        ClaudeConfig              `yaml:"claude"`
	Channels      map[string]*ChannelConfig `yaml:"channels"` // key = chat ID string
	Transcription TranscriptionConfig       `yaml:"transcription"`
	Log           LogConfig                 `yaml:"log"`
}

// FeishuConfig holds Feishu/Lark bot settings.
type FeishuConfig struct {
	AppID       string `yaml:"app_id"`
	AppSecret   string `yaml:"app_secret"`
	EncryptKey  string `yaml:"encrypt_key"`   // optional AES event encryption key
	UseWebhook  bool   `yaml:"use_webhook"`   // true = HTTP webhook mode (needs public IP); default false = WebSocket long connection
	WebhookPath string `yaml:"webhook_path"`  // HTTP path, default "/feishu" (webhook mode only)
	ListenAddr  string `yaml:"listen_addr"`   // e.g. ":8080" (webhook mode only)
}

// QQBotConfig holds QQ group bot settings.
// The bot connects outbound via WebSocket (no public IP required).
type QQBotConfig struct {
	AppID        string `yaml:"app_id"`        // AppID from bots.qq.com
	ClientSecret string `yaml:"client_secret"` // Client secret from bots.qq.com
}

// DingTalkConfig holds DingTalk (钉钉) stream bot settings.
// Stream mode opens an outbound WebSocket — no public IP required.
type DingTalkConfig struct {
	AppKey    string `yaml:"app_key"`    // AppKey from open.dingtalk.com
	AppSecret string `yaml:"app_secret"` // AppSecret from open.dingtalk.com
}

// WeWorkConfig holds WeCom (企业微信) webhook bot settings.
// Webhook mode requires a public IP or reverse proxy.
type WeWorkConfig struct {
	CorpID         string `yaml:"corp_id"`          // Enterprise CorpID
	AgentID        int    `yaml:"agent_id"`          // App Agent ID
	AgentSecret    string `yaml:"agent_secret"`      // App Agent Secret
	Token          string `yaml:"token"`             // Webhook token for signature verification
	EncodingAESKey string `yaml:"encoding_aes_key"`  // 43-char base64 AES key for decryption
	WebhookPath    string `yaml:"webhook_path"`      // HTTP path, default "/wework"
	ListenAddr     string `yaml:"listen_addr"`       // e.g. ":8081"
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
	if cfg.Feishu.WebhookPath == "" {
		cfg.Feishu.WebhookPath = "/feishu"
	}
	if cfg.Feishu.ListenAddr == "" {
		cfg.Feishu.ListenAddr = ":8080"
	}
	if cfg.WeWork.WebhookPath == "" {
		cfg.WeWork.WebhookPath = "/wework"
	}
	if cfg.WeWork.ListenAddr == "" {
		cfg.WeWork.ListenAddr = ":8081"
	}
	// Defaults for whisper-cpp sub-config.
	if cfg.Transcription.WhisperCPP.Bin == "" {
		cfg.Transcription.WhisperCPP.Bin = "whisper-cli"
	}
	// Groq model default applied in transcribe.New().

	return &cfg, nil
}
