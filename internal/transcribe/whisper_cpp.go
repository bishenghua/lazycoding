package transcribe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type whisperCPPTranscriber struct {
	cfg WhisperCPPConfig
	bin string
}

// Transcribe runs whisper-cli on audioPath and returns the transcribed text.
//
// whisper.cpp natively reads WAV, MP3, and FLAC.  For OGG/OPUS files sent by
// Telegram, it attempts an optional ffmpeg pre-conversion if ffmpeg is in PATH.
// If ffmpeg is absent the file is passed directly — recent whisper.cpp builds
// compiled with ffmpeg support can handle OGG natively.
func (t *whisperCPPTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	// Attempt OGG→WAV conversion when ffmpeg is available.
	// This is a best-effort step; if it fails we proceed with the original file.
	inputPath := audioPath
	cleanup := func() {}

	if needsConversion(audioPath) {
		if wav, cleanFn, err := ffmpegToWAV(ctx, audioPath); err == nil {
			inputPath = wav
			cleanup = cleanFn
		}
		// If ffmpeg is missing or fails, try the original file anyway.
	}
	defer cleanup()

	// whisper-cli writes text output to <outBase>.txt when given -otxt -of <outBase>.
	tmpDir, err := os.MkdirTemp("", "lazycoding-stt-")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outBase := filepath.Join(tmpDir, "out")

	args := []string{
		"-m", t.cfg.Model,
		"-f", inputPath,
		"-otxt",
		"-of", outBase,
	}
	if t.cfg.Language != "" {
		args = append(args, "-l", t.cfg.Language)
	}

	out, err := exec.CommandContext(ctx, t.bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper-cli: %w\noutput: %s", err, strings.TrimSpace(string(out)))
	}

	txt, err := os.ReadFile(outBase + ".txt")
	if err != nil {
		return "", fmt.Errorf("read whisper output: %w", err)
	}

	text := strings.TrimSpace(string(txt))
	text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
	text = strings.TrimSpace(text)

	if text == "" {
		return "", fmt.Errorf("语音内容为空或无法识别")
	}
	return text, nil
}

// needsConversion returns true for audio formats that whisper.cpp may not
// support without an ffmpeg-enabled build.
func needsConversion(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".ogg", ".opus", ".m4a", ".mp4", ".webm":
		return true
	}
	return false
}

// ffmpegToWAV converts audioPath to a 16 kHz mono WAV file using ffmpeg.
// Returns the WAV path and a cleanup func to delete it.
func ffmpegToWAV(ctx context.Context, audioPath string) (string, func(), error) {
	// Only attempt if ffmpeg is in PATH — don't add it as a hard dependency.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", func() {}, fmt.Errorf("ffmpeg not found")
	}

	wavPath := audioPath + ".wav"
	args := []string{
		"-i", audioPath,
		"-ar", "16000", // whisper.cpp expects 16 kHz
		"-ac", "1",     // mono
		"-f", "wav",
		"-y", // overwrite
		wavPath,
	}
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		return "", func() {}, fmt.Errorf("ffmpeg convert: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return wavPath, func() { os.Remove(wavPath) }, nil
}
