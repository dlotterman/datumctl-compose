// Package deploy creates, updates, and deletes a Compose project's Datum
// resources through datumctl, refusing to touch anything it does not own.
//
// Up works in two phases. First it reads every target resource and checks
// that each one is either missing or owned by the same Compose project and
// service. Only when every check passes does it write anything, so an
// ownership conflict never leaves the project half deployed.
package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/dlotterman/datumctl-compose/internal/datum"
	"github.com/dlotterman/datumctl-compose/internal/identity"
)

// Runner runs datumctl with args, writing stdin to it when stdin is not nil,
// and returns its standard output. *datum.Client implements it.
type Runner interface {
	Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

// Deployer applies and removes the resources of one Compose project.
type Deployer struct {
	Runner Runner
	// Out receives one line for each resource created, updated, or deleted.
	Out io.Writer
}

// resourceType is how datumctl names a kind on its command line.
type resourceType struct {
	singular, plural string
}

var (
	networks        = resourceType{"network", "networks"}
	workloads       = resourceType{"workload", "workloads"}
	networkServices = resourceType{"networkservice", "networkservices"}
	httpProxies     = resourceType{"httpproxy", "httpproxies"}
)

var resourceTypes = map[string]resourceType{
	datum.KindNetwork:        networks,
	datum.KindWorkload:       workloads,
	datum.KindNetworkService: networkServices,
	datum.KindHTTPProxy:      httpProxies,
}

// resource identifies one live Datum resource.
type resource struct {
	typ  resourceType
	name string
}

func (r resource) String() string {
	return r.typ.singular + " " + r.name
}

// CheckNetworksUsable returns an error unless every named Datum network
// exists and is not being deleted. Workloads attached to a missing or deleting
// network are accepted by Datum, but their instances never start.
func (d *Deployer) CheckNetworksUsable(ctx context.Context, names []string) error {
	for _, name := range names {
		live, err := d.Runner.Run(ctx, nil, "get", networks.singular, name, "--ignore-not-found", "-o", "json")
		if err != nil {
			return fmt.Errorf("checking Datum network %q: %w", name, err)
		}
		if len(bytes.TrimSpace(live)) == 0 {
			return fmt.Errorf("network %q does not exist in Datum; create it, or change the Compose file or --network", name)
		}
		meta, err := decodeMeta(live)
		if err != nil {
			return fmt.Errorf("reading Datum network %q: %w", name, err)
		}
		if meta.DeletionTimestamp != nil {
			return fmt.Errorf("network %q is being deleted in Datum; instances attached to it will not start", name)
		}
	}
	return nil
}

// Up creates or updates objects, all of which must belong to project. It
// also deletes the project's network services and HTTP proxies that are not
// in objects, such as the proxy of a service whose http-port was removed.
//
// Up never deletes a network. A network that is no longer in objects may
// still have instances attached while they move to their new network, so Up
// reports it and leaves its removal to Down.
func (d *Deployer) Up(ctx context.Context, project string, objects []datum.Object) error {
	changes, err := d.plan(ctx, objects)
	if err != nil {
		return err
	}
	stale, err := d.staleNetworking(ctx, project, changes)
	if err != nil {
		return err
	}
	unused, err := d.unusedNetworks(ctx, project, changes)
	if err != nil {
		return err
	}
	// Unpublish removed HTTP ports before changing the workloads.
	if err := d.delete(ctx, stale); err != nil {
		return err
	}
	for _, c := range changes {
		if err := d.apply(ctx, c); err != nil {
			return err
		}
	}
	for _, n := range unused {
		fmt.Fprintf(d.Out, "Kept %s: it is no longer in the Compose file; \"down\" deletes it\n", n)
	}
	return nil
}

// Down deletes every resource owned by project: proxies first, then network
// services, workloads, and finally the networks the workloads were attached to.
func (d *Deployer) Down(ctx context.Context, project string) error {
	// List everything before deleting anything, so that one foreign
	// resource stops the whole operation.
	var owned []resource
	for _, typ := range []resourceType{httpProxies, networkServices, workloads, networks} {
		items, err := d.listOwned(ctx, project, typ)
		if err != nil {
			return err
		}
		owned = append(owned, items...)
	}
	if len(owned) == 0 {
		fmt.Fprintln(d.Out, "No resources found for", project)
		return nil
	}
	return d.delete(ctx, owned)
}

// staleNetworking returns the project's network services and HTTP proxies
// that are not part of changes.
func (d *Deployer) staleNetworking(ctx context.Context, project string, changes []change) ([]resource, error) {
	desired := map[resource]bool{}
	for _, c := range changes {
		desired[c.resource()] = true
	}
	var stale []resource
	for _, typ := range []resourceType{httpProxies, networkServices} {
		items, err := d.listOwned(ctx, project, typ)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if !desired[item] {
				stale = append(stale, item)
			}
		}
	}
	return stale, nil
}

// unusedNetworks returns the project's networks that are not part of changes.
func (d *Deployer) unusedNetworks(ctx context.Context, project string, changes []change) ([]resource, error) {
	desired := map[resource]bool{}
	for _, c := range changes {
		desired[c.resource()] = true
	}
	items, err := d.listOwned(ctx, project, networks)
	if err != nil {
		return nil, err
	}
	var unused []resource
	for _, item := range items {
		if !desired[item] {
			unused = append(unused, item)
		}
	}
	return unused, nil
}

// listOwned lists the project's resources of one type. It fails if the
// label selector matched anything whose full ownership does not check out.
func (d *Deployer) listOwned(ctx context.Context, project string, typ resourceType) ([]resource, error) {
	data, err := d.Runner.Run(ctx, nil, "get", typ.plural, "-l", identity.Selector(project), "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata datum.ObjectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading %s list: %w", typ.plural, err)
	}
	result := make([]resource, 0, len(list.Items))
	for _, item := range list.Items {
		r := resource{typ, item.Metadata.Name}
		if r.name == "" || !identity.OwnsProject(item.Metadata, project) {
			return nil, fmt.Errorf("refusing to delete %s: ownership labels do not match", r)
		}
		result = append(result, r)
	}
	return result, nil
}

func (d *Deployer) delete(ctx context.Context, resources []resource) error {
	for _, r := range resources {
		if _, err := d.Runner.Run(ctx, nil, "delete", r.typ.singular, r.name); err != nil {
			return err
		}
		fmt.Fprintln(d.Out, "Deleted", r)
	}
	return nil
}
