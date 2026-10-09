package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dlotterman/datumctl-compose/internal/datum"
	"github.com/dlotterman/datumctl-compose/internal/identity"
)

const project = "deploy-test"

// fakeRunner stands in for datumctl.
type fakeRunner func(stdin []byte, args ...string) ([]byte, error)

func (f fakeRunner) Run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	return f(stdin, args...)
}

var emptyList = []byte(`{"items":[]}`)

// isList reports whether args is a label-selector query.
func isList(args []string) bool {
	return args[0] == "get" && slices.Contains(args, "-l")
}

func newWorkload(service string) *datum.Workload {
	return datum.NewWorkload(identity.Meta(project+"-"+service, project, service), datum.WorkloadSpec{})
}

// ownedMeta is the metadata of a live service resource that datumctl-compose
// created, as the API server returns it.
func ownedMeta(name, service string) datum.ObjectMeta {
	meta := identity.Meta(name, project, service)
	meta.UID, meta.ResourceVersion = "uid-"+service, "123"
	return meta
}

func liveJSON(t *testing.T, meta datum.ObjectMeta) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sentMeta(t *testing.T, stdin []byte) datum.ObjectMeta {
	t.Helper()
	var object struct {
		Metadata datum.ObjectMeta `json:"metadata"`
	}
	if err := yaml.Unmarshal(stdin, &object); err != nil {
		t.Fatal(err)
	}
	return object.Metadata
}

func TestUpChecksAllOwnersBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*datum.ObjectMeta)
	}{
		{"unmanaged", func(m *datum.ObjectMeta) { m.Labels = nil }},
		{"other manager", func(m *datum.ObjectMeta) { m.Labels[identity.ManagedByLabel] = "another-tool" }},
		{"other project", func(m *datum.ObjectMeta) { m.Labels[identity.ProjectLabel] = "other" }},
		{"other service", func(m *datum.ObjectMeta) { m.Labels[identity.ServiceLabel] = "different-service" }},
		{"conflicting annotation", func(m *datum.ObjectMeta) {
			m.Annotations = map[string]string{identity.ProjectAnnotation: "different-project"}
		}},
		{"deleting", func(m *datum.ObjectMeta) { v := "2026-10-07T00:00:00Z"; m.DeletionTimestamp = &v }},
		{"missing concurrency metadata", func(m *datum.ObjectMeta) { m.ResourceVersion = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := ownedMeta("deploy-test-two", "two")
			tc.change(&existing)
			var calls []string
			runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args[:3], " "))
				if args[0] != "get" {
					t.Fatalf("write before every owner was checked: %v", args)
				}
				if args[2] == "deploy-test-one" {
					return nil, nil
				}
				return liveJSON(t, existing), nil
			})
			var out bytes.Buffer
			d := &Deployer{Runner: runner, Out: &out}
			err := d.Up(context.Background(), project, []datum.Object{newWorkload("one"), newWorkload("two")})
			if err == nil {
				t.Fatal("expected rejection")
			}
			want := []string{"get workload deploy-test-one", "get workload deploy-test-two"}
			if !slices.Equal(calls, want) || out.Len() != 0 {
				t.Fatalf("calls=%v output=%q", calls, out.String())
			}
		})
	}
}

func TestUpCreatesOnlyMissingResourcesAndPinsOwnedUpdates(t *testing.T) {
	var calls []string
	runner := fakeRunner(func(stdin []byte, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		if isList(args) {
			return emptyList, nil
		}
		if args[0] == "get" {
			if !slices.Contains(args, "--ignore-not-found") {
				t.Fatalf("missing not-found handling: %v", args)
			}
			if args[2] == "deploy-test-one" {
				return nil, nil
			}
			return liveJSON(t, ownedMeta("deploy-test-two", "two")), nil
		}
		if !slices.Contains(args, "--field-manager=datumctl-compose") {
			t.Fatalf("writes must use a stable field manager: %v", args)
		}
		meta := sentMeta(t, stdin)
		switch args[0] {
		case "create":
			if meta.Name != "deploy-test-one" || meta.UID != "" || meta.ResourceVersion != "" {
				t.Fatalf("bad create metadata: %+v", meta)
			}
		case "apply":
			if !slices.Contains(args, "--server-side") {
				t.Fatal("update must send UID and resourceVersion to the server")
			}
			if meta.Name != "deploy-test-two" || meta.UID != "uid-two" || meta.ResourceVersion != "123" {
				t.Fatalf("unguarded update: %+v", meta)
			}
		default:
			t.Fatalf("unexpected command %v", args)
		}
		return nil, nil
	})
	var out bytes.Buffer
	d := &Deployer{Runner: runner, Out: &out}
	if err := d.Up(context.Background(), project, []datum.Object{newWorkload("one"), newWorkload("two")}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"get", "get", "get", "get", "get", "create", "apply"}; !slices.Equal(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
	if want := "Created workload deploy-test-one\nUpdated workload deploy-test-two\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

func TestUpDoesNotTreatReadErrorsAsAbsence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response []byte
		err      error
	}{
		{name: "forbidden", err: errors.New("forbidden")},
		{name: "malformed response", response: []byte("not json")},
		{name: "null response", response: []byte("null")},
		{name: "missing metadata", response: []byte("{}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
				if args[0] != "get" {
					t.Fatalf("unexpected write %v", args)
				}
				return tc.response, tc.err
			})
			d := &Deployer{Runner: runner, Out: &bytes.Buffer{}}
			if err := d.Up(context.Background(), project, []datum.Object{newWorkload("one")}); err == nil {
				t.Fatal("wanted error")
			}
		})
	}
}

func TestUpStopsOnConcurrentConflict(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint("exists=", exists), func(t *testing.T) {
			writes := 0
			runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
				if isList(args) {
					return emptyList, nil
				}
				if args[0] == "get" {
					if exists {
						return liveJSON(t, ownedMeta("deploy-test-one", "one")), nil
					}
					return nil, nil
				}
				writes++
				if exists && args[0] != "apply" || !exists && args[0] != "create" {
					t.Fatalf("unsafe fallback %v", args)
				}
				return nil, errors.New("concurrent resource change")
			})
			var out bytes.Buffer
			d := &Deployer{Runner: runner, Out: &out}
			err := d.Up(context.Background(), project, []datum.Object{newWorkload("one")})
			if err == nil || !strings.Contains(err.Error(), "concurrent resource change") || writes != 1 || out.Len() != 0 {
				t.Fatalf("error=%v writes=%d out=%s", err, writes, out.String())
			}
		})
	}
}

func publicService(name string) []datum.Object {
	meta := func() datum.ObjectMeta { return identity.Meta(project+"-"+name, project, name) }
	return []datum.Object{
		datum.NewWorkload(meta(), datum.WorkloadSpec{}),
		datum.NewNetworkService(meta(), datum.NetworkServiceSpec{}),
		datum.NewHTTPProxy(meta(), datum.HTTPProxySpec{}),
	}
}

func TestUpChecksNetworkingOwnersBeforeAnyWrite(t *testing.T) {
	var calls []string
	runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args[:2], " "))
		if args[0] != "get" {
			t.Fatalf("wrote before checking the proxy owner: %v", args)
		}
		if args[1] == "httpproxy" {
			foreign := ownedMeta("deploy-test-web", "web")
			foreign.Labels[identity.ManagedByLabel] = "another-tool"
			return liveJSON(t, foreign), nil
		}
		return nil, nil
	})
	var out bytes.Buffer
	d := &Deployer{Runner: runner, Out: &out}
	err := d.Up(context.Background(), project, publicService("web"))
	if err == nil || !strings.Contains(err.Error(), "refusing to update httpproxy deploy-test-web") {
		t.Fatalf("foreign proxy was accepted: %v", err)
	}
	if want := []string{"get workload", "get networkservice", "get httpproxy"}; !slices.Equal(calls, want) || out.Len() != 0 {
		t.Fatalf("calls=%v output=%q", calls, out.String())
	}
}

func TestUpCreatesInDependencyOrderWithStableFieldManager(t *testing.T) {
	var created []string
	runner := fakeRunner(func(stdin []byte, args ...string) ([]byte, error) {
		if isList(args) {
			return emptyList, nil
		}
		if args[0] == "get" {
			return nil, nil
		}
		if want := []string{"create", "-f", "-", "--field-manager=datumctl-compose"}; !slices.Equal(args, want) {
			t.Fatalf("args %v, want %v", args, want)
		}
		var object datum.TypeMeta
		if err := yaml.Unmarshal(stdin, &object); err != nil {
			t.Fatal(err)
		}
		created = append(created, object.Kind)
		return nil, nil
	})
	d := &Deployer{Runner: runner, Out: &bytes.Buffer{}}
	if err := d.Up(context.Background(), project, publicService("web")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Workload", "NetworkService", "HTTPProxy"}; !slices.Equal(created, want) {
		t.Fatalf("created %v, want %v", created, want)
	}
}

func TestUpDeletesStaleProxyBeforeWriting(t *testing.T) {
	var calls []string
	runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args[:2], " "))
		switch {
		case isList(args) && args[1] == "httpproxies":
			stale := ownedMeta("deploy-test-web", "web")
			return json.Marshal(map[string]any{"items": []any{map[string]any{"metadata": stale}}})
		case isList(args):
			return emptyList, nil
		case args[0] == "get":
			return nil, nil
		}
		return nil, nil
	})
	var out bytes.Buffer
	d := &Deployer{Runner: runner, Out: &out}
	objects := publicService("web")[:2] // http-port removed: no HTTPProxy
	if err := d.Up(context.Background(), project, objects); err != nil {
		t.Fatal(err)
	}
	want := []string{"get workload", "get networkservice", "get httpproxies", "get networkservices", "get networks", "delete httpproxy", "create -f", "create -f"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
	if !strings.HasPrefix(out.String(), "Deleted httpproxy deploy-test-web\n") {
		t.Fatalf("output %q", out.String())
	}
}

func TestStatusOnlyChange(t *testing.T) {
	before := []byte(`{"metadata":{"uid":"same","resourceVersion":"1","labels":{"owner":"me"}},"spec":{"value":"original"},"status":{"ready":false}}`)
	for _, tc := range []struct {
		name, after string
		safe        bool
	}{
		{"status", `{"metadata":{"uid":"same","resourceVersion":"2","labels":{"owner":"me"},"managedFields":[]},"spec":{"value":"original"},"status":{"ready":true}}`, true},
		{"spec", `{"metadata":{"uid":"same","resourceVersion":"2","labels":{"owner":"me"}},"spec":{"value":"changed"}}`, false},
		{"replacement", `{"metadata":{"uid":"different","resourceVersion":"2","labels":{"owner":"me"}},"spec":{"value":"original"}}`, false},
		{"owner", `{"metadata":{"uid":"same","resourceVersion":"2","labels":{"owner":"other"}},"spec":{"value":"original"}}`, false},
		{"no version change", string(before), false},
		{"absent", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version, safe := statusOnlyChange(before, []byte(tc.after))
			if safe != tc.safe || safe && version != "2" {
				t.Fatalf("version=%q safe=%v", version, safe)
			}
		})
	}
}

func TestUpdateRetriesOnlyStatusRacesAndHasLimit(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		t.Run(fmt.Sprint("succeeds=", succeeds), func(t *testing.T) {
			reads, writes := 0, 0
			runner := fakeRunner(func(stdin []byte, args ...string) ([]byte, error) {
				if args[0] == "get" {
					reads++
					live := ownedMeta("deploy-test-one", "one")
					live.ResourceVersion = fmt.Sprint(123 + reads)
					return liveJSON(t, live), nil
				}
				writes++
				meta := sentMeta(t, stdin)
				if meta.UID != "uid-one" || meta.ResourceVersion != fmt.Sprint(122+writes) {
					t.Fatalf("bad preconditions: %+v", meta)
				}
				if succeeds && writes == 2 {
					return nil, nil
				}
				return nil, errors.New("resource version conflict")
			})
			object := newWorkload("one")
			object.Metadata.UID, object.Metadata.ResourceVersion = "uid-one", "123"
			c := change{object: object, typ: workloads, exists: true, observed: liveJSON(t, ownedMeta("deploy-test-one", "one"))}
			d := &Deployer{Runner: runner, Out: &bytes.Buffer{}}
			err := d.update(context.Background(), c)
			if succeeds && (err != nil || writes != 2 || reads != 1) || !succeeds && (err == nil || writes != 3 || reads != 2) {
				t.Fatalf("err=%v writes=%d reads=%d", err, writes, reads)
			}
		})
	}
}

func newNetwork(name string) *datum.Network {
	return datum.NewNetwork(identity.NetworkMeta(project+"-"+name, project, name))
}

func ownedNetworkMeta(name string) datum.ObjectMeta {
	meta := identity.NetworkMeta(project+"-"+name, project, name)
	meta.UID, meta.ResourceVersion = "uid-"+name, "7"
	return meta
}

func TestUpAppliesNetworksBeforeTheWorkloadsThatUseThem(t *testing.T) {
	var created []string
	runner := fakeRunner(func(stdin []byte, args ...string) ([]byte, error) {
		if isList(args) {
			return emptyList, nil
		}
		if args[0] == "get" {
			return nil, nil
		}
		var object datum.TypeMeta
		if err := yaml.Unmarshal(stdin, &object); err != nil {
			t.Fatal(err)
		}
		created = append(created, object.Kind)
		return nil, nil
	})
	d := &Deployer{Runner: runner, Out: &bytes.Buffer{}}
	if err := d.Up(context.Background(), project, []datum.Object{newNetwork("backend"), newWorkload("one")}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Network", "Workload"}; !slices.Equal(created, want) {
		t.Fatalf("created %v, want %v", created, want)
	}
}

func TestUpRefusesToAdoptAForeignNetwork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*datum.ObjectMeta)
	}{
		{"unmanaged", func(m *datum.ObjectMeta) { m.Labels, m.Annotations = nil, nil }},
		{"other project", func(m *datum.ObjectMeta) { m.Labels[identity.ProjectLabel] = "other" }},
		{"other network", func(m *datum.ObjectMeta) { m.Annotations[identity.NetworkAnnotation] = "frontend" }},
		{"a service with the same name", func(m *datum.ObjectMeta) {
			*m = ownedMeta(m.Name, "backend")
			m.Annotations = map[string]string{identity.ProjectAnnotation: project, identity.ServiceAnnotation: "backend"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := ownedNetworkMeta("backend")
			tc.change(&live)
			runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
				if args[0] != "get" {
					t.Fatalf("wrote before checking the network owner: %v", args)
				}
				return liveJSON(t, live), nil
			})
			d := &Deployer{Runner: runner, Out: &bytes.Buffer{}}
			err := d.Up(context.Background(), project, []datum.Object{newNetwork("backend")})
			if err == nil || !strings.Contains(err.Error(), "refusing to update network deploy-test-backend") {
				t.Fatalf("foreign network was accepted: %v", err)
			}
		})
	}
}

func TestUpKeepsUnusedNetworksAndSaysSo(t *testing.T) {
	runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
		switch {
		case args[0] == "get" && args[1] == "networks":
			return json.Marshal(map[string]any{"items": []any{
				map[string]any{"metadata": ownedNetworkMeta("backend")},
				map[string]any{"metadata": ownedNetworkMeta("old")},
			}})
		case isList(args):
			return emptyList, nil
		case args[0] == "get" && args[2] == "deploy-test-backend":
			return liveJSON(t, ownedNetworkMeta("backend")), nil
		case args[0] == "get":
			return nil, nil
		case args[0] == "delete":
			t.Fatalf("Up must not delete networks: %v", args)
		}
		return nil, nil
	})
	var out bytes.Buffer
	d := &Deployer{Runner: runner, Out: &out}
	if err := d.Up(context.Background(), project, []datum.Object{newNetwork("backend"), newWorkload("one")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Kept network deploy-test-old") || strings.Contains(out.String(), "Kept network deploy-test-backend") {
		t.Fatalf("output %q", out.String())
	}
}

func TestDownDeletesNetworksLast(t *testing.T) {
	var deleted []string
	runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
		if isList(args) {
			var metas []any
			switch args[1] {
			case "networks":
				metas = []any{map[string]any{"metadata": ownedNetworkMeta("backend")}}
			case "workloads":
				metas = []any{map[string]any{"metadata": ownedMeta("deploy-test-one", "one")}}
			}
			return json.Marshal(map[string]any{"items": metas})
		}
		deleted = append(deleted, args[1])
		return nil, nil
	})
	var out bytes.Buffer
	d := &Deployer{Runner: runner, Out: &out}
	if err := d.Down(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if want := []string{"workload", "network"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted %v, want %v", deleted, want)
	}
}

func TestCheckNetworksUsable(t *testing.T) {
	deleting := "2026-10-09T01:25:45Z"
	for _, tc := range []struct {
		name    string
		live    []byte
		err     error
		wantErr string
	}{
		{name: "ready", live: []byte(`{"metadata":{"name":"net"}}`)},
		{name: "missing", live: nil, wantErr: `network "net" does not exist in Datum`},
		{name: "deleting", live: []byte(`{"metadata":{"name":"net","deletionTimestamp":"` + deleting + `"}}`), wantErr: `network "net" is being deleted in Datum`},
		{name: "forbidden", err: errors.New("forbidden"), wantErr: "forbidden"},
		{name: "malformed", live: []byte("not json"), wantErr: "reading Datum network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			runner := fakeRunner(func(_ []byte, args ...string) ([]byte, error) {
				asked = append(asked, strings.Join(args, " "))
				return tc.live, tc.err
			})
			d := &Deployer{Runner: runner, Out: &bytes.Buffer{}}
			err := d.CheckNetworksUsable(context.Background(), []string{"net"})
			if (err == nil) != (tc.wantErr == "") || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if want := []string{"get network net --ignore-not-found -o json"}; !slices.Equal(asked, want) {
				t.Fatalf("asked %v", asked)
			}
		})
	}
	d := &Deployer{Runner: fakeRunner(func([]byte, ...string) ([]byte, error) {
		t.Fatal("no networks to check, so no datumctl call")
		return nil, nil
	}), Out: &bytes.Buffer{}}
	if err := d.CheckNetworksUsable(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
