// Package compose loads a Compose project the same way docker compose does:
// file discovery, COMPOSE_* variables, .env files, interpolation, override
// files, and profiles all come from compose-go.
package compose

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// Options are the Compose command-line flags. Empty fields fall back to the
// COMPOSE_* environment variables and then to Compose's defaults.
type Options struct {
	Files       []string // -f
	Profiles    []string // --profile
	ProjectName string   // -p
}

// Load finds, reads, and merges the Compose files and returns the project
// with only its active services.
func Load(ctx context.Context, opts Options) (*types.Project, error) {
	projectOptions, err := cli.NewProjectOptions(opts.Files,
		cli.WithOsEnv,
		cli.WithEnvFiles(),
		cli.WithDotEnv,
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
		// Discovery (or COMPOSE_FILE) may change the project directory. Load its
		// .env too, keeping shell and initial .env values at higher precedence.
		cli.WithEnvFiles(),
		cli.WithDotEnv,
		cli.WithDefaultProfiles(opts.Profiles...),
		cli.WithName(opts.ProjectName),
	)
	if err != nil {
		return nil, err
	}
	if len(projectOptions.ConfigPaths) == 0 {
		return nil, errors.New("no Compose file found; use -f")
	}
	workingDir, err := projectOptions.GetWorkingDir()
	if err != nil {
		return nil, err
	}
	config, err := projectOptions.ReadConfigFiles(ctx, workingDir, projectOptions)
	if err != nil {
		return nil, err
	}
	config.Environment = projectOptions.Environment

	// compose-go rejects unknown service keys, so rename http-port before it
	// validates the files. See rewriteHTTPPort.
	removedHTTPPort := map[string]bool{}
	for i, file := range config.ConfigFiles {
		content, nulled, err := rewriteHTTPPort(file.Content)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.Filename, err)
		}
		config.ConfigFiles[i].Content = content
		for service, isNull := range nulled {
			removedHTTPPort[service] = isNull
		}
	}

	project, err := loader.LoadWithContext(ctx, *config,
		loader.WithProfiles(activeProfiles(opts, projectOptions)),
		withProjectName(projectOptions, workingDir),
	)
	if err != nil {
		return nil, err
	}

	// Compose's extension merge keeps the earlier value when an override sets
	// it to null. For http-port, null in a later file means "stop publishing".
	for name, removed := range removedHTTPPort {
		if service, ok := project.Services[name]; ok && removed {
			delete(service.Extensions, HTTPPortExtension)
			project.Services[name] = service
		}
	}
	for _, file := range config.ConfigFiles {
		project.ComposeFiles = append(project.ComposeFiles, file.Filename)
	}
	return project, nil
}

func activeProfiles(opts Options, projectOptions *cli.ProjectOptions) []string {
	if len(opts.Profiles) > 0 {
		return opts.Profiles
	}
	return strings.Split(projectOptions.Environment["COMPOSE_PROFILES"], ",")
}

// withProjectName picks the project name the way docker compose does: -p,
// then COMPOSE_PROJECT_NAME, then the name: field, then the directory name.
func withProjectName(projectOptions *cli.ProjectOptions, workingDir string) func(*loader.Options) {
	name := projectOptions.Name
	if name == "" {
		name = projectOptions.Environment["COMPOSE_PROJECT_NAME"]
	}
	return func(options *loader.Options) {
		if name != "" {
			options.SetProjectName(name, true)
		} else {
			options.SetProjectName(loader.NormalizeProjectName(filepath.Base(workingDir)), false)
		}
	}
}
