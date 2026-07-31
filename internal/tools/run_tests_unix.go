//go:build !windows

package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func executePythonScript(ctx context.Context, dir, scriptPath, input string) (string, error) {
	pythonExe := "python3"
	if _, err := exec.LookPath("python3"); err != nil {
		pythonExe = "python"
	}

	cmd := exec.CommandContext(ctx, pythonExe, scriptPath)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdin bytes.Buffer
	stdin.WriteString(input)
	cmd.Stdin = &stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Start()
	if err != nil {
		return "", fmt.Errorf("process start error: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("time limit exceeded")
		}
		return "", ctx.Err()

	case err := <-done:
		if err != nil {
			errStr := stderr.String()
			if errStr == "" {
				errStr = err.Error()
			}
			return "", fmt.Errorf("runtime error: %s", errStr)
		}
		return stdout.String(), nil
	}
}