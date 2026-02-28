//go:build !whisper

package transcribe

import (
	"context"
	"fmt"
)

// whisperNativeTranscriber is a stub used when the binary is built without -tags whisper.
type whisperNativeTranscriber struct{}

func newWhisperNative(_ WhisperNativeConfig) (*whisperNativeTranscriber, error) {
	return nil, fmt.Errorf(
		"whisper-native backend requires CGo build tag:\n" +
			"  go build -tags whisper ./cmd/lazycoding/\n" +
			"First build compiles whisper.cpp (~3-5 min). Subsequent builds are fast.")
}

func (t *whisperNativeTranscriber) Transcribe(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("whisper-native: not compiled (build with -tags whisper)")
}
