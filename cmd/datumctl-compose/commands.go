package main

import (
	"fmt"
	"io"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/dlotterman/datumctl-compose/internal/compose"
	"github.com/dlotterman/datumctl-compose/internal/datum"
	"github.com/dlotterman/datumctl-compose/internal/deploy"
	"github.com/dlotterman/datumctl-compose/internal/identity"
	"github.com/dlotterman/datumctl-compose/internal/translate"
)

// globalFlags are accepted by every subcommand.
type globalFlags struct {
	compose      compose.Options
	datumProject string
	verbose      bool
}

func newRootCommand(stdout, stderr io.Writer) *cobra.Command {
	var flags globalFlags
	root := &cobra.Command{
		Use:   "compose",
		Short: "Deploy Compose applications to Datum Compute",
		// datumctl runs this binary as "datumctl compose"; show that in help.
		Annotations: map[string]string{cobra.CommandDisplayNameAnnotation: "datumctl compose"},
		Long: `Deploy Compose applications to Datum Compute.

Unsupported Compose features cause an error. --best-effort drops them and
prints each omission to stderr. Review "config" output before "up".`,
		SilenceUsage:  true, // a failed deployment is not a usage mistake
		SilenceErrors: true, // main prints the error
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)

	f := root.PersistentFlags()
	f.StringArrayVarP(&flags.compose.Files, "file", "f", nil, "Compose file (repeatable)")
	f.StringVarP(&flags.compose.ProjectName, "project-name", "p", "", "Compose project name")
	f.StringArrayVar(&flags.compose.Profiles, "profile", nil, "enable a Compose profile (repeatable)")
	f.StringVar(&flags.datumProject, "datum-project", "", "Datum project (default: datumctl's active context)")
	f.BoolVarP(&flags.verbose, "verbose", "v", false, "print each datumctl command and its input to stderr")

	root.AddCommand(
		newConfigCommand(&flags),
		newUpCommand(&flags),
		newPsCommand(&flags),
		newDownCommand(&flags),
		newVersionCommand(),
	)
	return root
}

func newConfigCommand(flags *globalFlags) *cobra.Command {
	var opts translate.Options
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print the Datum resources that up would apply",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, result, err := loadAndTranslate(cmd, flags, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for i, object := range result.Objects() {
				data, err := yaml.Marshal(object)
				if err != nil {
					return err
				}
				if i > 0 {
					fmt.Fprintln(out, "---")
				}
				if _, err := out.Write(data); err != nil {
					return err
				}
			}
			return nil
		},
	}
	addTranslateFlags(cmd, &opts)
	return cmd
}

func newUpCommand(flags *globalFlags) *cobra.Command {
	var opts translate.Options
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or update the project's Datum resources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, result, err := loadAndTranslate(cmd, flags, opts)
			if err != nil {
				return err
			}
			deployer := newDeployer(cmd, flags)
			if err := deployer.CheckNetworksUsable(cmd.Context(), result.ExistingNetworks); err != nil {
				return err
			}
			if err := deployer.Up(cmd.Context(), project.Name, result.Objects()); err != nil {
				return err
			}
			// Instances can take minutes to start, or stall on platform
			// problems; a successful up only means Datum accepted the change.
			fmt.Fprintln(cmd.ErrOrStderr(), `up does not wait for readiness; check progress with "datumctl compose ps"`)
			return nil
		},
	}
	addTranslateFlags(cmd, &opts)
	return cmd
}

func newPsCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List the project's networks, workloads, network services, and HTTP proxies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := loadProject(cmd, flags)
			if err != nil {
				return err
			}
			table, err := newClient(cmd, flags).Run(cmd.Context(), nil,
				"get", "networks,workloads,networkservices,httpproxies", "-l", identity.Selector(project.Name))
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(table)
			return err
		},
	}
}

func newDownCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Delete every Datum resource owned by the project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := loadProject(cmd, flags)
			if err != nil {
				return err
			}
			return newDeployer(cmd, flags).Down(cmd.Context(), project.Name)
		},
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version)
		},
	}
}

func addTranslateFlags(cmd *cobra.Command, opts *translate.Options) {
	f := cmd.Flags()
	f.StringVar(&opts.Location, "location", "", "Datum location to run the workloads in (required)")
	f.StringVar(&opts.Network, "network", "", "attach every workload to this existing Datum network, instead of creating networks from the Compose file")
	f.StringVar(&opts.InstanceType, "instance-type", "", "Datum instance type (default: the platform default)")
	f.StringVar(&opts.RuntimeClass, "runtime-class", "general-purpose", "Datum runtime class")
	f.BoolVar(&opts.BestEffort, "best-effort", false, "drop unsupported Compose settings, with a warning for each")
	_ = cmd.MarkFlagRequired("location") // cannot fail: the flag exists
}

func loadProject(cmd *cobra.Command, flags *globalFlags) (*types.Project, error) {
	project, err := compose.Load(cmd.Context(), flags.compose)
	if err != nil {
		return nil, err
	}
	if len(project.Services) == 0 {
		return nil, fmt.Errorf("compose project %q has no active services", project.Name)
	}
	return project, nil
}

// loadAndTranslate loads the project and translates it, printing a warning
// for every setting --best-effort dropped.
func loadAndTranslate(cmd *cobra.Command, flags *globalFlags, opts translate.Options) (*types.Project, *translate.Result, error) {
	project, err := loadProject(cmd, flags)
	if err != nil {
		return nil, nil, err
	}
	result, err := translate.Translate(project, opts)
	if err != nil {
		return nil, nil, err
	}
	for _, issue := range result.Dropped {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning: dropping", issue)
	}
	return project, result, nil
}

func newClient(cmd *cobra.Command, flags *globalFlags) *datum.Client {
	client := datum.NewClient(flags.datumProject)
	client.Stderr = cmd.ErrOrStderr()
	if flags.verbose {
		client.Trace = cmd.ErrOrStderr()
	}
	return client
}

func newDeployer(cmd *cobra.Command, flags *globalFlags) *deploy.Deployer {
	return &deploy.Deployer{Runner: newClient(cmd, flags), Out: cmd.OutOrStdout()}
}
