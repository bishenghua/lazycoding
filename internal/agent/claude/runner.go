package claude

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"lazycoding/internal/agent"
	"lazycoding/internal/config"
)

// Runner implements agent.Agent using the local claude CLI.
type Runner struct {
	cfg *config.ClaudeConfig
}

// New creates a Runner from the global claude section of the config.
func New(cfg *config.ClaudeConfig) *Runner {
	return &Runner{cfg: cfg}
}

// Stream starts a claude subprocess and streams events from its stdout.
//
// Per-request overrides in req.WorkDir and req.ExtraFlags take priority over
// the global runner configuration, enabling per-channel project directories.
func (r *Runner) Stream(ctx context.Context, req agent.StreamRequest) (<-chan agent.Event, error) {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = r.cfg.WorkDir
	}

	args := r.buildArgs(req)

	cmd := exec.CommandContext(ctx, "claude", args...)
	if workDir != "" {
		cmd.Dir = workDir
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	ch := make(chan agent.Event, 32)

	go func() {
		defer close(ch)

		scanner := bufio.NewScanner(stdout)
		// 4 MB buffer to handle large tool outputs.
		scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			for _, ev := range ParseLineMulti(line) {
				select {
				case ch <- ev:
				case <-ctx.Done():
					// Drain stdout so the process can exit cleanly.
					for scanner.Scan() {
					}
					cmd.Wait() //nolint:errcheck
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			select {
			case ch <- agent.Event{Kind: agent.EventKindError, Err: fmt.Errorf("read stdout: %w", err)}:
			case <-ctx.Done():
			}
		}

		if err := cmd.Wait(); err != nil {
			se := stderrBuf.String()
			var wrapped error
			if se != "" {
				wrapped = fmt.Errorf("claude exited: %w\nstderr: %s", err, se)
			} else {
				wrapped = fmt.Errorf("claude exited: %w", err)
			}
			select {
			case ch <- agent.Event{Kind: agent.EventKindError, Err: wrapped}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}

// buildArgs assembles the claude CLI argument list for the given request.
// --dangerously-skip-permissions is always included.
// req.ExtraFlags overrides r.cfg.ExtraFlags when non-nil.
func (r *Runner) buildArgs(req agent.StreamRequest) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}

	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}

	// Per-request extra flags take precedence over the global default.
	extraFlags := r.cfg.ExtraFlags
	if req.ExtraFlags != nil {
		extraFlags = req.ExtraFlags
	}
	args = append(args, extraFlags...)
	args = append(args, req.Prompt)
	return args
}
