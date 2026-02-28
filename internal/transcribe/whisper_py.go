package transcribe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// whisperPyTranscriber uses the openai-whisper Python CLI.
// Install: pip install openai-whisper
// openai-whisper invokes ffmpeg internally, so it accepts OGG/OPUS natively.
type whisperPyTranscriber struct {
	cfg   WhisperPyConfig
	bin   string
	model string
}

func (t *whisperPyTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "lazycoding-stt-")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	args := []string{
		audioPath,
		"--model", t.model,
		"--output_format", "txt",
		"--output_dir", tmpDir,
		"--verbose", "False",
	}
	if t.cfg.Language != "" {
		args = append(args, "--language", t.cfg.Language)
	}

	out, err := exec.CommandContext(ctx, t.bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// openai-whisper names the output file after the input file's base name.
	base := strings.TrimSuffix(filepath.Base(audioPath), filepath.Ext(audioPath))
	txt, err := os.ReadFile(filepath.Join(tmpDir, base+".txt"))
	if err != nil {
		return "", fmt.Errorf("read output: %w", err)
	}

	text := strings.TrimSpace(string(txt))
	text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("transcription result is empty")
	}
	return text, nil
}
