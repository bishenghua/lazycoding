package transcribe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const groqEndpoint = "https://api.groq.com/openai/v1/audio/transcriptions"

type groqTranscriber struct {
	cfg   GroqConfig
	model string
}

// Transcribe uploads the audio file to Groq's Whisper API and returns the text.
// Groq accepts OGG/OPUS natively, so no format conversion is needed.
func (t *groqTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("open audio: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// Attach the audio file.
	part, err := w.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("copy audio: %w", err)
	}

	// Required fields.
	w.WriteField("model", t.model)           //nolint:errcheck
	w.WriteField("response_format", "text")  //nolint:errcheck

	// Optional language hint (improves accuracy and speed).
	if t.cfg.Language != "" {
		w.WriteField("language", t.cfg.Language) //nolint:errcheck
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqEndpoint, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("groq request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("groq API %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	text := strings.TrimSpace(string(respBody))
	if text == "" {
		return "", fmt.Errorf("语音内容为空或无法识别")
	}
	return text, nil
}
