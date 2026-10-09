package translate

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/dlotterman/datumctl-compose/internal/datum"
	"github.com/dlotterman/datumctl-compose/internal/identity"
)

// defaultNetwork is the network Compose attaches a service to when it lists
// none.
const defaultNetwork = "default"

// networkPlan says which Datum network each Compose network maps to.
type networkPlan struct {
	// datumName maps a Compose network to the Datum network its services use.
	datumName map[string]string
	// created are the Datum networks to create, one per Compose network that
	// the active services use and that is not external.
	created []*datum.Network
	// existing are Datum networks the services use but this plugin does not
	// create: external Compose networks, or the --network override.
	existing []string
	issues   []Issue
}

// planNetworks decides which Datum network each Compose network maps to.
//
// A Compose network that an active service uses becomes a Datum network named
// "<project>-<network>", which the plugin creates and owns. An external
// Compose network names an existing Datum network instead. With
// Options.Network set, every service attaches to that one existing network.
//
// It fails when a service lists more than one network: Datum Compute attaches
// an instance to a single network, and dropping one would silently change
// which services can reach each other, so --best-effort does not allow it.
func planNetworks(project *types.Project, opts Options) (*networkPlan, error) {
	if err := checkOneNetworkPerService(project); err != nil {
		return nil, err
	}

	used := map[string]bool{}
	for _, service := range project.Services {
		used[serviceNetwork(service)] = true
	}

	plan := &networkPlan{datumName: map[string]string{}}
	owner := map[string]string{} // Datum network name -> Compose network, to detect collisions
	for _, key := range slices.Sorted(maps.Keys(used)) {
		config := project.Networks[key]
		path := "networks." + key

		if opts.Network != "" {
			plan.datumName[key] = opts.Network
			if key != defaultNetwork || len(networkSettings(project, key, config)) > 0 {
				plan.issues = append(plan.issues, Issue{path, fmt.Sprintf(
					"this network is not created because --network attaches every service to Datum network %q", opts.Network)})
			}
			continue
		}

		for _, setting := range networkSettings(project, key, config) {
			reason := "no Datum mapping"
			if setting == "internal" {
				reason = "Compose network isolation cannot be preserved; the Datum network does not enforce it"
			}
			plan.issues = append(plan.issues, Issue{path + "." + setting, reason})
		}
		if bool(config.External) {
			plan.datumName[key] = config.Name
			plan.existing = append(plan.existing, config.Name)
			continue
		}
		name := identity.NetworkName(project.Name, key)
		if other, exists := owner[name]; exists {
			return nil, fmt.Errorf("networks.%s and networks.%s map to the same Datum network %q; rename a network", other, key, name)
		}
		owner[name] = key
		plan.datumName[key] = name
		plan.created = append(plan.created, datum.NewNetwork(identity.NetworkMeta(name, project.Name, key)))
	}
	if opts.Network != "" {
		plan.existing = []string{opts.Network}
	}
	return plan, nil
}

// serviceNetwork returns the single Compose network the service attaches to.
// checkOneNetworkPerService has already rejected services with several.
func serviceNetwork(service types.ServiceConfig) string {
	for name := range service.Networks {
		return name
	}
	return defaultNetwork
}

// checkOneNetworkPerService returns an error naming every service that is
// attached to more than one network.
func checkOneNetworkPerService(project *types.Project) error {
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		networks := slices.Sorted(maps.Keys(project.Services[name].Networks))
		if len(networks) > 1 {
			lines = append(lines, fmt.Sprintf("services.%s is attached to %d networks (%s)", name, len(networks), strings.Join(networks, ", ")))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("a service cannot join several Compose networks, because Datum Compute attaches an instance to one network at a time:\n  %s\n"+
		"Attach each service to a single network. --best-effort cannot drop networks, because that would change which services can reach each other",
		strings.Join(lines, "\n  "))
}

// networkSettings returns the settings of a Compose network that have no
// Datum equivalent. For an external network, external and name are not
// settings: they say which Datum network to use.
func networkSettings(project *types.Project, key string, config types.NetworkConfig) []string {
	external := bool(config.External)
	var settings []string
	for setting := range setFields(config) {
		switch {
		case setting == "name" && !external && config.Name == project.Name+"_"+key:
			continue // compose-go names every network "<project>_<network>"
		case setting == "name" && external, setting == "external":
			continue
		}
		settings = append(settings, setting)
	}
	slices.Sort(settings)
	return settings
}
