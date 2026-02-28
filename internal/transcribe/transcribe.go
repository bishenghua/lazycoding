// Package transcribe provides speech-to-text transcription via pluggable backends.
//
// Supported backends (configured via transcription.backend in config.yaml):
//
//   - "groq"           – Groq cloud API (free tier, zero local install, needs API key)
//   - "whisper-cpp"    – Local whisper.cpp binary (brew install whisper-cpp)
//   - "whisper-native" – Embedded whisper.cpp via CGo (go build -tags whisper)
//   - "whisper"        – openai-whisper Python CLI (pip install openai-whisper)
package transcribe

import (
	"context"
	"fmt"
)

// Transcriber is the interface for speech-to-text backends.
type Transcriber interface {
	// Transcribe converts the audio file at audioPath to plain text.
	// audioPath is typically a temporary OGG/OPUS file from Telegram.
	Transcribe(ctx context.Context, audioPath string) (string, error)
}

// Config is the top-level transcription configuration.
type Config struct {
	Enabled       bool                `yaml:"enabled"`
	Backend       string              `yaml:"backend"` // "groq" | "whisper-cpp" | "whisper-native" | "whisper"
	WhisperCPP    WhisperCPPConfig    `yaml:"whisper_cpp"`
	WhisperPy     WhisperPyConfig     `yaml:"whisper_py"`
	WhisperNative WhisperNativeConfig `yaml:"whisper_native"`
	Groq          GroqConfig          `yaml:"groq"`
}

// WhisperNativeConfig configures the embedded whisper.cpp CGo backend.
// Requires building with: go build -tags whisper ./cmd/lazycoding/
type WhisperNativeConfig struct {
	// Model is a model name ("base", "small", "large-v3-turbo") or an absolute
	// path to a local .ggml file. Named models are auto-downloaded to ModelDir.
	// Default: "base" (~140 MB)
	Model string `yaml:"model"`

	// ModelDir is the directory where auto-downloaded models are cached.
	// Default: ~/.cache/lazycoding/whisper/
	ModelDir string `yaml:"model_dir"`

	// Language is the spoken language code ("zh", "en", …).
	// Empty = auto-detect.
	Language string `yaml:"language"`
}

// WhisperPyConfig configures the openai-whisper Python CLI backend.
// Install: pip install openai-whisper  (requires Python + ffmpeg)
type WhisperPyConfig struct {
	// Bin is the whisper executable name or full path. Default: "whisper"
	Bin string `yaml:"bin"`

	// Model size: tiny | base | small | medium | large. Default: "base"
	Model string `yaml:"model"`

	// Language code ("zh", "en", …). Empty = auto-detect.
	Language string `yaml:"language"`
}

// WhisperCPPConfig configures the local whisper.cpp CLI backend.
type WhisperCPPConfig struct {
	// Bin is the whisper-cli executable name or full path.
	// Default: "whisper-cli"  (brew install whisper-cpp installs this name)
	Bin string `yaml:"bin"`

	// Model is the required path to the GGML model file.
	// Download: whisper-download-ggml-model base
	// or: https://huggingface.co/ggerganov/whisper.cpp/tree/main
	Model string `yaml:"model"`

	// Language is the spoken language code ("zh", "en", …).
	// Empty = auto-detect (slower).
	Language string `yaml:"language"`
}

// GroqConfig configures the Groq cloud Whisper API backend.
type GroqConfig struct {
	// APIKey from https://console.groq.com (free signup, generous free tier).
	APIKey string `yaml:"api_key"`

	// Model to use. Default: "whisper-large-v3-turbo" (fast, accurate).
	// Options: "whisper-large-v3-turbo", "whisper-large-v3", "distil-whisper-large-v3-en"
	Model string `yaml:"model"`

	// Language is the spoken language code ("zh", "en", …).
	// Empty = auto-detect.
	Language string `yaml:"language"`
}

// New creates a Transcriber from cfg.
// Returns nil (no error) when cfg.Enabled is false.
func New(cfg Config) (Transcriber, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	switch cfg.Backend {
	case "groq":
		if cfg.Groq.APIKey == "" {
			return nil, fmt.Errorf("transcription.groq.api_key is required when backend is \"groq\"")
		}
		model := cfg.Groq.Model
		if model == "" {
			model = "whisper-large-v3-turbo"
		}
		return &groqTranscriber{cfg: cfg.Groq, model: model}, nil

	case "whisper-cpp":
		if cfg.WhisperCPP.Model == "" {
			return nil, fmt.Errorf(
				"transcription.whisper_cpp.model (path to .ggml file) is required when backend is \"whisper-cpp\"\n" +
					"  Download: whisper-download-ggml-model base\n" +
					"  Or visit: https://huggingface.co/ggerganov/whisper.cpp")
		}
		bin := cfg.WhisperCPP.Bin
		if bin == "" {
			bin = "whisper-cli"
		}
		return &whisperCPPTranscriber{cfg: cfg.WhisperCPP, bin: bin}, nil

	case "whisper-native":
		return newWhisperNative(cfg.WhisperNative)

	case "whisper", "openai-whisper", "":
		bin := cfg.WhisperPy.Bin
		if bin == "" {
			bin = "whisper"
		}
		model := cfg.WhisperPy.Model
		if model == "" {
			model = "base"
		}
		return &whisperPyTranscriber{cfg: cfg.WhisperPy, bin: bin, model: model}, nil

	default:
		return nil, fmt.Errorf("unknown transcription backend %q (supported: \"groq\", \"whisper-cpp\", \"whisper-native\", \"whisper\")", cfg.Backend)
	}
}
