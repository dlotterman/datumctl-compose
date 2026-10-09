package datum

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Client runs the datumctl executable. Every call goes through Run, so
// setting Trace shows exactly what the plugin asked datumctl to do.
type Client struct {
	// Binary is the datumctl executable to run.
	Binary string
	// Project is passed to every call as --project when it is set. When it
	// is empty, datumctl uses the project from its active context.
	Project string
	// Stderr receives datumctl's own warnings. When a call fails, its stderr
	// is part of the returned error instead, so that a failure the caller
	// retries successfully does not print anything.
	Stderr io.Writer
	// Trace, when set, receives each command line and its stdin before it
	// runs, and datumctl's stderr afterwards.
	Trace io.Writer
}

// NewClient returns a client for the datumctl on PATH, or for the executable
// named by the DATUMCTL_BIN environment variable.
func NewClient(project string) *Client {
	binary := os.Getenv("DATUMCTL_BIN")
	if binary == "" {
		binary = "datumctl"
	}
	return &Client{Binary: binary, Project: project, Stderr: os.Stderr}
}

// Run runs datumctl with args, writing stdin to it when stdin is not nil, and
// returns its standard output.
func (c *Client) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	if c.Project != "" {
		args = append([]string{"--project", c.Project}, args...)
	}
	commandLine := c.Binary + " " + quoteArgs(args)
	if c.Trace != nil {
		fmt.Fprintf(c.Trace, "+ %s\n", commandLine)
		if stdin != nil {
			fmt.Fprintf(c.Trace, "%s\n", bytes.TrimSpace(stdin))
		}
	}

	cmd := exec.CommandContext(ctx, c.Binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if c.Trace != nil && stderr.Len() > 0 {
		fmt.Fprintf(c.Trace, "%s\n", bytes.TrimSpace(stderr.Bytes()))
	}
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return nil, fmt.Errorf("%s: %w\n%s", commandLine, err, message)
		}
		return nil, fmt.Errorf("%s: %w", commandLine, err)
	}
	if c.Stderr != nil {
		c.Stderr.Write(stderr.Bytes())
	}
	return out, nil
}

// quoteArgs joins args so that the result can be pasted into a shell.
func quoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\n'\"\\$`;&|<>()*?[]{}!#~") {
			arg = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
		quoted[i] = arg
	}
	return strings.Join(quoted, " ")
}
