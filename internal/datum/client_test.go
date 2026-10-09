package datum

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDatumctl writes a shell script that prints to stdout and stderr and
// exits with the given status.
func fakeDatumctl(t *testing.T, stdout, stderr string, status int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "datumctl")
	script := "#!/bin/sh\nprintf '%s' '" + stdout + "'\nprintf '%s' '" + stderr + "' >&2\nexit " + string(rune('0'+status)) + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunFailureNamesTheCommandAndKeepsStderrInTheError(t *testing.T) {
	var warnings bytes.Buffer
	c := &Client{Binary: fakeDatumctl(t, "", "the object has been modified", 1), Project: "p", Stderr: &warnings}
	_, err := c.Run(context.Background(), nil, "apply", "-f", "-")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"--project p apply -f -", "exit status 1", "the object has been modified"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if warnings.Len() != 0 {
		t.Fatalf("a failed call must not also print its stderr: %q", warnings.String())
	}
}

func TestRunSuccessPassesWarningsThrough(t *testing.T) {
	var warnings bytes.Buffer
	c := &Client{Binary: fakeDatumctl(t, "output", "a warning", 0), Stderr: &warnings}
	out, err := c.Run(context.Background(), nil, "get", "workloads")
	if err != nil || string(out) != "output" || warnings.String() != "a warning" {
		t.Fatalf("out=%q err=%v warnings=%q", out, err, warnings.String())
	}
}

func TestTraceShowsCommandsInputAndStderr(t *testing.T) {
	var trace bytes.Buffer
	c := &Client{Binary: fakeDatumctl(t, "", "oops", 1), Trace: &trace}
	_, _ = c.Run(context.Background(), []byte("kind: Workload\n"), "create", "-f", "-")
	want := "kind: Workload\n"
	if !strings.Contains(trace.String(), "create -f -\n"+want) || !strings.Contains(trace.String(), "oops") {
		t.Fatalf("trace %q", trace.String())
	}
}

func TestQuoteArgs(t *testing.T) {
	if got := quoteArgs([]string{"get", "-l", "a=b,c=d", "it's here", ""}); got != `get -l a=b,c=d 'it'\''s here' ''` {
		t.Fatalf("quoteArgs = %s", got)
	}
}
