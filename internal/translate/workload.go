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

// defaultCapabilities is Docker's default capability set. Without an
// explicit security context, Datum reports instances as ready while their
// containers never start.
var defaultCapabilities = []string{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL",
	"NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID",
}

// buildWorkload translates one service. It also returns the container ports
// so the caller can build the matching NetworkService.
func buildWorkload(meta datum.ObjectMeta, name, network string, service types.ServiceConfig, opts Options) (*datum.Workload, []datum.NamedPort, []Issue, error) {
	if service.Image == "" {
		return nil, nil, nil, fmt.Errorf("services.%s.image is required; build-only services need a published image", name)
	}
	if !hasRegistryHost(service.Image) {
		return nil, nil, nil, fmt.Errorf("services.%s.image must include a registry host (for example docker.io/library/nginx:latest)", name)
	}
	replicas := service.GetScale()
	if replicas <= 0 {
		return nil, nil, nil, fmt.Errorf("services.%s: Datum requires scale/deploy.replicas to be greater than zero; refusing to start a service configured with zero replicas", name)
	}

	issues := unsupportedServiceFields(name, service)
	env, envIssues := environment(name, service.Environment)
	ports, portIssues := containerPorts(name, service.Ports)
	issues = append(issues, envIssues...)
	issues = append(issues, portIssues...)

	container := datum.Container{
		Name:    identity.ContainerName(name),
		Image:   service.Image,
		Command: []string(service.Entrypoint), // Compose entrypoint is the container command,
		Args:    []string(service.Command),    // and Compose command is its arguments.
		Env:     env,
		Ports:   ports,
		SecurityContext: datum.SecurityContext{
			AllowPrivilegeEscalation: false,
			Capabilities:             datum.Capabilities{Add: defaultCapabilities, Drop: []string{"ALL"}},
			SeccompProfile:           datum.SeccompProfile{Type: "RuntimeDefault"},
		},
	}

	spec := datum.WorkloadSpec{
		Template: datum.InstanceTemplate{Spec: datum.InstanceSpec{
			Runtime: datum.Runtime{
				Class:     opts.RuntimeClass,
				Resources: datum.RuntimeResources{InstanceType: opts.InstanceType},
				Sandbox:   datum.Sandbox{Containers: []datum.Container{container}},
			},
			NetworkInterfaces: []datum.NetworkInterface{{
				Name:    "eth0",
				Network: datum.Ref{Name: network},
			}},
		}},
		Placements: []datum.Placement{{
			Name:          "default",
			Locations:     []datum.Ref{{Name: opts.Location}},
			ScaleSettings: datum.ScaleSettings{MinReplicas: replicas},
		}},
	}
	return datum.NewWorkload(meta, spec), ports, issues, nil
}

// environment returns the service's variables sorted by name. A variable
// listed without a value (such as "- DEBUG" with DEBUG unset) is an issue.
func environment(service string, vars types.MappingWithEquals) ([]datum.EnvVar, []Issue) {
	var env []datum.EnvVar
	var issues []Issue
	for _, key := range slices.Sorted(maps.Keys(vars)) {
		value := vars[key]
		if value == nil {
			issues = append(issues, Issue{"services." + service + ".environment." + key, "value is unset"})
			continue
		}
		env = append(env, datum.EnvVar{Name: key, Value: *value})
	}
	return env, issues
}

// containerPorts translates the target side of each Compose port. Ports are
// named port-<index>, using the index in the Compose list, so the same port
// keeps its name when other ports are added or dropped after it.
func containerPorts(service string, ports []types.ServicePortConfig) ([]datum.NamedPort, []Issue) {
	var result []datum.NamedPort
	var issues []Issue
	for i, port := range ports {
		path := fmt.Sprintf("services.%s.ports[%d]", service, i)
		if port.HostIP != "" {
			issues = append(issues, Issue{path + ".host_ip", "host IP publishing is unavailable"})
		}
		if port.Target == 0 {
			issues = append(issues, Issue{path, "target port is missing"})
			continue
		}
		protocol := strings.ToUpper(port.Protocol)
		if protocol == "" {
			protocol = "TCP"
		}
		if protocol != "TCP" && protocol != "UDP" {
			issues = append(issues, Issue{path + ".protocol", "unsupported protocol"})
			continue
		}
		if port.Published != "" {
			issues = append(issues, Issue{path + ".published", "host publishing has no equivalent; port remains internal"})
		}
		result = append(result, datum.NamedPort{Name: fmt.Sprintf("port-%d", i), Port: port.Target, Protocol: protocol})
	}
	return result, issues
}

// hasRegistryHost follows the image reference convention: the first component
// must be localhost or contain a dot or colon to identify an explicit registry.
func hasRegistryHost(image string) bool {
	registry, _, hasPath := strings.Cut(image, "/")
	if !hasPath {
		return false
	}
	return registry == "localhost" || strings.ContainsAny(registry, ".:")
}
