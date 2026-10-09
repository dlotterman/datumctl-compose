// Command datumctl-compose is a datumctl plugin that deploys Compose
// applications to Datum Compute. datumctl runs it as "datumctl compose".
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is the plugin version. A release build can override it with
// -ldflags "-X main.version=...".
var version = "v0.1.0"

func main() {
	// datumctl asks every plugin to describe itself before running it.
	if len(os.Args) > 1 && os.Args[1] == "--plugin-manifest" {
		if err := writePluginManifest(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCommand(stdout, stderr)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func writePluginManifest(w io.Writer) error {
	return json.NewEncoder(w).Encode(struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		Description   string `json:"description"`
		APIVersion    int    `json:"api_version"`
		MinAPIVersion int    `json:"min_api_version"`
	}{
		Name:          "compose",
		Version:       version,
		Description:   "Deploy Compose applications to Datum Compute",
		APIVersion:    1,
		MinAPIVersion: 1,
	})
}
