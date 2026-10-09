package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"sigs.k8s.io/yaml"

	"github.com/dlotterman/datumctl-compose/internal/datum"
	"github.com/dlotterman/datumctl-compose/internal/identity"
)

// maxStatusRetries limits how often update retries after the resource's
// status, but nothing else, changed underneath it.
const maxStatusRetries = 2

// change is one planned write.
type change struct {
	object datum.Object
	typ    resourceType
	// exists is true for an update and false for a create.
	exists bool
	// observed is the live resource, as JSON, when exists is true.
	observed []byte
}

func (c change) resource() resource {
	return resource{c.typ, c.object.GetMetadata().Name}
}

// plan reads every object's live resource and decides whether to create or
// update it. It returns an error, before anything is written, if any live
// resource belongs to someone else or cannot be updated safely.
//
// For updates, plan copies the live UID and resourceVersion into the object.
// The API server then rejects the write if the resource was replaced or
// changed after it was checked.
func (d *Deployer) plan(ctx context.Context, objects []datum.Object) ([]change, error) {
	var changes []change
	seen := map[resource]bool{}
	for _, object := range objects {
		meta := object.GetMetadata()
		typ, ok := resourceTypes[object.GetKind()]
		if !ok {
			return nil, fmt.Errorf("unsupported kind %q", object.GetKind())
		}
		c := change{object: object, typ: typ}
		if meta.Name == "" {
			return nil, fmt.Errorf("%s has no name", typ.singular)
		}
		if seen[c.resource()] {
			return nil, fmt.Errorf("duplicate %s", c.resource())
		}
		seen[c.resource()] = true

		live, err := d.Runner.Run(ctx, nil, "get", typ.singular, meta.Name, "--ignore-not-found", "-o", "json")
		if err != nil {
			return nil, fmt.Errorf("checking ownership of %s: %w", c.resource(), err)
		}
		if len(bytes.TrimSpace(live)) > 0 {
			current, err := decodeMeta(live)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", c.resource(), err)
			}
			if err := checkUpdatable(c.resource(), current, *meta); err != nil {
				return nil, err
			}
			meta.UID = current.UID
			meta.ResourceVersion = current.ResourceVersion
			c.exists = true
			c.observed = live
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// checkUpdatable returns an error unless the live resource is owned by the
// same Compose project and service as the desired one and is safe to update.
func checkUpdatable(r resource, live, desired datum.ObjectMeta) error {
	project := desired.Annotations[identity.ProjectAnnotation]
	owned := identity.OwnsService(live, project, desired.Annotations[identity.ServiceAnnotation])
	if network, isNetwork := desired.Annotations[identity.NetworkAnnotation]; isNetwork {
		owned = identity.OwnsNetwork(live, project, network)
	}
	switch {
	case live.Name != desired.Name || !owned:
		return fmt.Errorf("refusing to update %s: it is not owned by this Compose project", r)
	case live.DeletionTimestamp != nil:
		return fmt.Errorf("%s is being deleted; wait for deletion to finish before running up", r)
	case live.UID == "" || live.ResourceVersion == "":
		return fmt.Errorf("%s has no UID or resourceVersion; refusing an unguarded update", r)
	}
	return nil
}

func (d *Deployer) apply(ctx context.Context, c change) error {
	if c.exists {
		if err := d.update(ctx, c); err != nil {
			return fmt.Errorf("updating %s: %w; check the resource before rerunning up", c.resource(), err)
		}
		fmt.Fprintln(d.Out, "Updated", c.resource())
		return nil
	}
	if err := d.create(ctx, c); err != nil {
		return fmt.Errorf("creating %s: %w; check the resource before rerunning up", c.resource(), err)
	}
	fmt.Fprintln(d.Out, "Created", c.resource())
	return nil
}

// create fails if the resource appeared since plan checked for it.
func (d *Deployer) create(ctx context.Context, c change) error {
	data, err := yaml.Marshal(c.object)
	if err != nil {
		return err
	}
	_, err = d.Runner.Run(ctx, data, "create", "-f", "-", "--field-manager="+identity.ManagerName)
	return err
}

// update server-side applies the object, which carries the UID and
// resourceVersion that plan observed. Controllers update a resource's status
// all the time, which also changes its resourceVersion. When the write fails
// and only status or bookkeeping changed, update retries with the new
// resourceVersion. Any other change means someone else is editing the
// resource, and update stops so a person can look at it.
func (d *Deployer) update(ctx context.Context, c change) error {
	meta := c.object.GetMetadata()
	observed := c.observed
	for attempt := 0; ; attempt++ {
		data, err := yaml.Marshal(c.object)
		if err != nil {
			return err
		}
		_, writeErr := d.Runner.Run(ctx, data, "apply", "-f", "-", "--server-side",
			"--field-manager="+identity.ManagerName, "--force-conflicts")
		if writeErr == nil || attempt == maxStatusRetries {
			return writeErr
		}

		live, err := d.Runner.Run(ctx, nil, "get", c.typ.singular, meta.Name, "-o", "json")
		if err != nil {
			return fmt.Errorf("%w (could not recheck resource: %v)", writeErr, err)
		}
		version, ok := statusOnlyChange(observed, live)
		if !ok {
			return writeErr
		}
		meta.ResourceVersion = version
		observed = live
	}
}

// statusOnlyChange reports whether after differs from before only in status,
// managedFields, and resourceVersion, and returns after's resourceVersion.
func statusOnlyChange(before, after []byte) (string, bool) {
	oldObject, oldVersion := withoutBookkeeping(before)
	newObject, newVersion := withoutBookkeeping(after)
	if oldVersion == "" || newVersion == "" || oldVersion == newVersion {
		return newVersion, false
	}
	return newVersion, reflect.DeepEqual(oldObject, newObject)
}

// withoutBookkeeping decodes a live resource, removes the fields that change
// without anyone editing it, and returns it with its resourceVersion.
func withoutBookkeeping(raw []byte) (map[string]any, string) {
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil {
		return nil, ""
	}
	meta, ok := object["metadata"].(map[string]any)
	if !ok {
		return nil, ""
	}
	version, _ := meta["resourceVersion"].(string)
	delete(object, "status")
	delete(meta, "resourceVersion")
	delete(meta, "managedFields")
	return object, version
}

func decodeMeta(live []byte) (datum.ObjectMeta, error) {
	var object struct {
		Metadata datum.ObjectMeta `json:"metadata"`
	}
	err := json.Unmarshal(live, &object)
	return object.Metadata, err
}
