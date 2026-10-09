package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func composeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigRendersWorkload(t *testing.T) {
	p := composeFile(t, "name: demo\nservices:\n  api:\n    image: docker.io/library/nginx:1.27\n    environment:\n      MESSAGE: hello\n")
	var out, errs bytes.Buffer
	if err := run(context.Background(), []string{"config", "-f", p, "--location", "us-west-1"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Workload", "name: demo-api", "name: MESSAGE", "value: hello", "class: general-purpose", "minReplicas: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
	if errs.Len() > 0 {
		t.Fatalf("unexpected warnings: %s", errs.String())
	}
}

func TestConfigPrintsDroppedSettingsAsWarnings(t *testing.T) {
	p := composeFile(t, "services:\n  api:\n    image: docker.io/library/nginx:1.27\n    volumes:\n      - ./data:/data\n")
	var out, errs bytes.Buffer
	err := run(context.Background(), []string{"config", "-f", p, "--location", "us-west-1"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "services.api.volumes") {
		t.Fatalf("wanted volume error, got %v", err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"config", "-f", p, "--location", "us-west-1", "--best-effort"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs.String(), "Warning: dropping services.api.volumes") {
		t.Fatalf("warning missing: %s", errs.String())
	}
	if strings.Contains(out.String(), "volume") {
		t.Fatalf("volume incorrectly translated: %s", out.String())
	}
}

func TestConfigRequiresLocation(t *testing.T) {
	p := composeFile(t, "services:\n  api:\n    image: docker.io/library/nginx:1.27\n")
	err := run(context.Background(), []string{"config", "-f", p}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("wanted missing --location error, got %v", err)
	}
}
