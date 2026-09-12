// Package execx runs external commands (ipmitool, smartctl) with a bounded
// timeout and captures stdout/stderr separately so callers get clean parse
// input and useful error messages.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds any single external command invocation. ipmitool
// talking to a real BMC can be slow (observed ~1.5s for a full SDR walk on
// real hardware); smartctl on a spun-down disk can be slower still.
const DefaultTimeout = 10 * time.Second

// Run executes bin with args, waiting at most timeout (or DefaultTimeout if
// timeout <= 0). It returns trimmed stdout on success. On failure the error
// includes stderr output so callers don't need to thread it through.
func Run(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s %s: timed out after %s", bin, strings.Join(args, " "), timeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
