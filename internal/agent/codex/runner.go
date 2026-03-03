package codex

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os/exec"

	"github.com/bishenghua/lazycoding/internal/agent"
	"github.com/bishenghua/lazycoding/internal/config"
)

// Runner implements agent.Agent using the local codex CLI.
type Runner struct {
	cfg       *config.CodexConfig
	globalCfg *config.ClaudeConfig // shares work_dir and timeout_sec with Claude
}

// New creates a Runner for the codex backend.
func New(cfg *config.CodexConfig, globalCfg *config.ClaudeConfig) *Runner {
	return &Runner{cfg: cfg, globalCfg: globalCfg}
}

// Stream starts a codex subprocess and streams events from its stdout.
//
// codex exec --json --ask-for-approval never --sandbox workspace-write
//            [--cd <workdir>] [--session <id>] [extra_flags...] <prompt>
//
// Per-request overrides in req.WorkDir and req.ExtraFlags take priority over
// the global runner configuration, enabling per-channel project directories.
func (r *Runner) Stream(ctx context.Context, req agent.StreamRequest) (<-chan agent.Event, error) {
	workDir := cmp.Or(req.WorkDir, r.globalCfg.WorkDir)

	args := r.buildArgs(req, workDir)

	cmd := exec.CommandContext(ctx, "codex", args...)
	// codex uses --cd flag for working directory instead of cmd.Dir.

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}

	ch := make(chan agent.Event, 32)

	go func() {
		defer close(ch)

		var sessionID string

		scanner := bufio.NewScanner(stdout)
		// 4 MB buffer to handle large tool outputs.
		scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			for _, ev := range ParseLine(line, &sessionID) {
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
				wrapped = fmt.Errorf("codex exited: %w\nstderr: %s", err, se)
			} else {
				wrapped = fmt.Errorf("codex exited: %w", err)
			}
			select {
			case ch <- agent.Event{Kind: agent.EventKindError, Err: wrapped}:
			case <-ctx.Done():
			}
		} else {
			// Emit a result event so the orchestrator saves the session ID.
			select {
			case ch <- agent.Event{Kind: agent.EventKindResult, SessionID: sessionID}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}

// buildArgs assembles the codex CLI argument list for the given request.
func (r *Runner) buildArgs(req agent.StreamRequest, workDir string) []string {
	args := []string{
		"exec",
		"--json",
		"--ask-for-approval", "never",
		"--sandbox", "workspace-write",
	}

	if workDir != "" {
		args = append(args, "--cd", workDir)
	}

	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	}

	// Per-request extra flags take precedence over the backend default which
	// takes precedence over the global claude extra_flags.
	extraFlags := r.globalCfg.ExtraFlags
	if r.cfg.ExtraFlags != nil {
		extraFlags = r.cfg.ExtraFlags
	}
	if req.ExtraFlags != nil {
		extraFlags = req.ExtraFlags
	}
	args = append(args, extraFlags...)
	args = append(args, req.Prompt)
	return args
}
