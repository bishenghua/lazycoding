//go:build whisper

package transcribe

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	whisper "github.com/ggerganov/whisper.cpp/bindings/go/pkg/whisper"
)

const (
	huggingFaceURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	whisperRate    = 16000 // whisper.cpp requires 16kHz mono PCM
)

// whisperNativeTranscriber uses the whisper.cpp Go CGo bindings for fully embedded STT.
// Build with: go build -tags whisper ./cmd/lazycoding/
type whisperNativeTranscriber struct {
	cfg   WhisperNativeConfig
	model whisper.Model
}

func newWhisperNative(cfg WhisperNativeConfig) (*whisperNativeTranscriber, error) {
	modelPath, err := resolveModel(cfg)
	if err != nil {
		return nil, err
	}
	model, err := whisper.New(modelPath)
	if err != nil {
		return nil, fmt.Errorf("whisper-native: load model %s: %w", modelPath, err)
	}
	return &whisperNativeTranscriber{cfg: cfg, model: model}, nil
}

func (t *whisperNativeTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	// Convert audio to 16kHz mono WAV via ffmpeg.
	wavPath, cleanup, err := toWAV16k(ctx, audioPath)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// Decode WAV → []float32.
	samples, err := readWAVFloat32(wavPath)
	if err != nil {
		return "", fmt.Errorf("read wav: %w", err)
	}

	wctx, err := t.model.NewContext()
	if err != nil {
		return "", fmt.Errorf("whisper context: %w", err)
	}

	lang := t.cfg.Language
	if lang == "" {
		lang = "auto"
	}
	if err := wctx.SetLanguage(lang); err != nil {
		// Non-multilingual models don't support language setting; ignore.
		_ = err
	}

	if err := wctx.Process(samples, nil, nil, nil); err != nil {
		return "", fmt.Errorf("whisper process: %w", err)
	}

	var sb strings.Builder
	for {
		seg, err := wctx.NextSegment()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("whisper segment: %w", err)
		}
		sb.WriteString(strings.TrimSpace(seg.Text))
		sb.WriteString(" ")
	}

	text := strings.TrimSpace(sb.String())
	text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("transcription result is empty")
	}
	return text, nil
}

// toWAV16k converts any audio file to a 16kHz mono 16-bit WAV using ffmpeg.
func toWAV16k(ctx context.Context, src string) (path string, cleanup func(), err error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", nil, fmt.Errorf("ffmpeg not found (required for whisper-native): %w", err)
	}
	f, err := os.CreateTemp("", "lazycoding-wav-*.wav")
	if err != nil {
		return "", nil, err
	}
	f.Close()
	dst := f.Name()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-i", src,
		"-ar", "16000", "-ac", "1", "-f", "wav",
		dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(dst)
		return "", nil, fmt.Errorf("ffmpeg: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return dst, func() { os.Remove(dst) }, nil
}

// readWAVFloat32 reads 16-bit PCM from a WAV file and returns float32 samples.
func readWAVFloat32(path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Skip to data chunk (minimal RIFF/WAV parsing).
	header := make([]byte, 44)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("wav header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	// Find "data" chunk (may not start at byte 36 for some encoders).
	dataSize, err := findWAVDataChunk(f, header)
	if err != nil {
		return nil, err
	}

	nSamples := int(dataSize) / 2
	samples := make([]float32, nSamples)
	buf := make([]byte, 2)
	for i := range samples {
		if _, err := io.ReadFull(f, buf); err != nil {
			return samples[:i], nil
		}
		s := int16(binary.LittleEndian.Uint16(buf))
		samples[i] = float32(s) / math.MaxInt16
	}
	return samples, nil
}

// findWAVDataChunk seeks past any non-data chunks and returns the data chunk size.
// header is the first 44 bytes already read.
func findWAVDataChunk(f *os.File, header []byte) (uint32, error) {
	// Standard layout: data chunk starts at byte 36.
	if string(header[36:40]) == "data" {
		return binary.LittleEndian.Uint32(header[40:44]), nil
	}
	// Non-standard: scan for "data" chunk.
	f.Seek(12, io.SeekStart) // after "RIFF....WAVE"
	chunk := make([]byte, 8)
	for {
		if _, err := io.ReadFull(f, chunk); err != nil {
			return 0, fmt.Errorf("wav: data chunk not found")
		}
		size := binary.LittleEndian.Uint32(chunk[4:8])
		if string(chunk[0:4]) == "data" {
			return size, nil
		}
		f.Seek(int64(size), io.SeekCurrent)
	}
}

// resolveModel returns the path to the GGML model file, downloading it if needed.
func resolveModel(cfg WhisperNativeConfig) (string, error) {
	name := cfg.Model
	if name == "" {
		name = "base"
	}
	// If it looks like an absolute path to an existing file, use it directly.
	if filepath.IsAbs(name) || strings.HasSuffix(name, ".bin") {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}

	// Model directory: configured or ~/.cache/lazycoding/whisper/
	modelDir := cfg.ModelDir
	if modelDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot find home dir: %w", err)
		}
		modelDir = filepath.Join(home, ".cache", "lazycoding", "whisper")
	}
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return "", fmt.Errorf("create model dir: %w", err)
	}

	// Normalise name to "ggml-{name}.bin".
	filename := name
	if !strings.HasPrefix(filename, "ggml-") {
		filename = "ggml-" + filename
	}
	if !strings.HasSuffix(filename, ".bin") {
		filename += ".bin"
	}
	modelPath := filepath.Join(modelDir, filename)

	if _, err := os.Stat(modelPath); err == nil {
		return modelPath, nil // already cached
	}

	url := huggingFaceURL + filename
	fmt.Printf("[whisper-native] downloading model %s → %s\n", filename, modelPath)
	if err := downloadFile(url, modelPath); err != nil {
		return "", fmt.Errorf("download model %s: %w", filename, err)
	}
	return modelPath, nil
}

// downloadFile downloads a URL to a local file with a 30-minute timeout.
func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, dest)
}
