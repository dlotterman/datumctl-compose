// Package translate turns a Compose project into Datum resources.
//
// Every active service becomes a Workload. A service with ports also gets a
// private NetworkService, and a service with http-port gets an HTTPProxy that
// publishes that port on the internet. Each Compose network the services use
// becomes a Datum Network, and each workload attaches to the network of its
// service (see planNetworks).
//
// Compose settings with no Datum equivalent are errors unless
// Options.BestEffort is set, in which case they are dropped and reported in
// Result.Dropped.
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

// Options are the Datum settings applied to every service.
type Options struct {
	Location string
	// Network, when set, is an existing Datum network that every workload
	// attaches to. No networks are created, and the Compose file's own
	// networks are ignored. When empty, networks come from the Compose file.
	Network      string
	InstanceType string // empty means the platform default
	RuntimeClass string
	BestEffort   bool
}

// Issue is a Compose setting that has no Datum equivalent.
type Issue struct {
	Path   string // Compose path, such as services.web.volumes
	Reason string
}

func (i Issue) String() string {
	return i.Path + ": " + i.Reason
}

// UnsupportedError lists every unsupported setting found when
// Options.BestEffort is not set.
type UnsupportedError struct {
	Issues []Issue
}

func (e *UnsupportedError) Error() string {
	lines := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		lines[i] = issue.String()
	}
	return fmt.Sprintf("unsupported Compose features:\n  %s\nUse --best-effort to drop them", strings.Join(lines, "\n  "))
}

// Result holds the translated resources, each list in service-name order.
type Result struct {
	Networks        []*datum.Network
	Workloads       []*datum.Workload
	NetworkServices []*datum.NetworkService
	HTTPProxies     []*datum.HTTPProxy

	// ExistingNetworks are the Datum networks the workloads attach to that
	// this plugin did not create: external Compose networks, or Options.Network.
	// They should exist, and not be deleting, before the workloads are applied.
	ExistingNetworks []string

	// Dropped lists the unsupported settings left out under BestEffort.
	Dropped []Issue
}

// Objects returns every resource in the order they should be applied:
// networks, then the workloads that attach to them, then the network services
// that select the workloads, then the proxies that route to those services.
func (r *Result) Objects() []datum.Object {
	var objects []datum.Object
	for _, n := range r.Networks {
		objects = append(objects, n)
	}
	for _, w := range r.Workloads {
		objects = append(objects, w)
	}
	for _, n := range r.NetworkServices {
		objects = append(objects, n)
	}
	for _, h := range r.HTTPProxies {
		objects = append(objects, h)
	}
	return objects
}

// Translate converts every active service in project.
func Translate(project *types.Project, opts Options) (*Result, error) {
	networks, err := planNetworks(project, opts)
	if err != nil {
		return nil, err
	}
	result := &Result{Networks: networks.created, ExistingNetworks: networks.existing}
	issues := networks.issues
	serviceForName := map[string]string{} // resource name -> service, to detect collisions

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service := project.Services[name]
		resourceName := identity.WorkloadName(project.Name, name)
		if other, exists := serviceForName[resourceName]; exists {
			return nil, fmt.Errorf("services.%s and services.%s map to the same workload %q; rename a service", other, name, resourceName)
		}
		serviceForName[resourceName] = name

		// Each resource gets its own copy so that none share label maps.
		meta := func() datum.ObjectMeta { return identity.Meta(resourceName, project.Name, name) }
		workload, ports, serviceIssues, err := buildWorkload(meta(), name, networks.datumName[serviceNetwork(service)], service, opts)
		if err != nil {
			return nil, err
		}
		issues = append(issues, serviceIssues...)
		result.Workloads = append(result.Workloads, workload)

		httpPort, err := httpPort(name, service)
		if err != nil {
			return nil, err
		}
		tcpPorts := tcpOnly(ports)
		if len(tcpPorts) == 0 {
			continue
		}
		result.NetworkServices = append(result.NetworkServices, buildNetworkService(meta(), tcpPorts))
		if httpPort != 0 {
			result.HTTPProxies = append(result.HTTPProxies, buildHTTPProxy(meta(), portName(tcpPorts, httpPort)))
		}
	}

	slices.SortFunc(issues, func(a, b Issue) int { return strings.Compare(a.Path, b.Path) })
	if len(issues) > 0 && !opts.BestEffort {
		return nil, &UnsupportedError{Issues: issues}
	}
	result.Dropped = issues
	return result, nil
}
