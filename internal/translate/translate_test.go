package translate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dlotterman/datumctl-compose/internal/compose"
	"github.com/dlotterman/datumctl-compose/internal/identity"
)

// render loads a Compose file with the given body and translates it.
func render(t *testing.T, body string, bestEffort bool) (*Result, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	project, err := compose.Load(context.Background(), compose.Options{Files: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	return Translate(project, Options{
		Location:     "us-west-1",
		RuntimeClass: "general-purpose",
		BestEffort:   bestEffort,
	})
}

// droppedPaths returns the paths of the issues in an UnsupportedError, or of
// the settings dropped under BestEffort.
func droppedPaths(result *Result, err error) []string {
	var issues []Issue
	var unsupported *UnsupportedError
	switch {
	case errors.As(err, &unsupported):
		issues = unsupported.Issues
	case result != nil:
		issues = result.Dropped
	}
	var paths []string
	for _, issue := range issues {
		paths = append(paths, issue.Path)
	}
	return paths
}

func TestRendersWorkload(t *testing.T) {
	result, err := render(t, "name: demo\nservices:\n  api:\n    image: docker.io/library/nginx:1.27\n    environment:\n      MESSAGE: hello\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Workloads) != 1 || len(result.NetworkServices) != 0 || len(result.HTTPProxies) != 0 {
		t.Fatalf("unexpected resources: %+v", result)
	}
	w := result.Workloads[0]
	container := w.Spec.Template.Spec.Runtime.Sandbox.Containers[0]
	if w.Metadata.Name != "demo-api" || container.Env[0].Name != "MESSAGE" || container.Env[0].Value != "hello" ||
		w.Spec.Template.Spec.Runtime.Class != "general-purpose" || w.Spec.Placements[0].ScaleSettings.MinReplicas != 1 {
		t.Fatalf("unexpected workload: %+v", w)
	}
	security := container.SecurityContext
	if security.AllowPrivilegeEscalation || security.SeccompProfile.Type != "RuntimeDefault" || !slices.Equal(security.Capabilities.Drop, []string{"ALL"}) {
		t.Fatalf("missing container security settings required for a running instance: %+v", security)
	}
}

func TestOriginalNamesArePreserved(t *testing.T) {
	project := strings.Repeat("p", 80)
	first, second := strings.Repeat("s", 80)+"a", strings.Repeat("s", 80)+"b"
	result, err := render(t, fmt.Sprintf("name: %s\nservices:\n  %s:\n    image: docker.io/library/nginx:1.27\n  %s:\n    image: docker.io/library/nginx:1.27\n", project, first, second), false)
	if err != nil {
		t.Fatal(err)
	}
	for i, service := range []string{first, second} {
		annotations := result.Workloads[i].Metadata.Annotations
		if annotations[identity.ProjectAnnotation] != project || annotations[identity.ServiceAnnotation] != service {
			t.Fatalf("lost original identity: %v", annotations)
		}
	}
}

func TestZeroReplicasDoesNotStartADisabledService(t *testing.T) {
	for _, bestEffort := range []bool{false, true} {
		for _, field := range []string{"scale: 0", "deploy: {replicas: 0}"} {
			_, err := render(t, "name: zero\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    "+field+"\n", bestEffort)
			if err == nil || !strings.Contains(err.Error(), "greater than zero") {
				t.Fatalf("bestEffort=%v, %s: expected rejection, got %v", bestEffort, field, err)
			}
		}
	}
}

func TestHashNameCollisionIsRejected(t *testing.T) {
	generated := identity.WorkloadName("demo", "api_web")
	colliding := strings.TrimPrefix(generated, "demo-")
	body := fmt.Sprintf("name: demo\nservices:\n  api_web:\n    image: docker.io/library/nginx:1.27\n  %s:\n    image: docker.io/library/nginx:1.27\n", colliding)
	for _, bestEffort := range []bool{false, true} {
		_, err := render(t, body, bestEffort)
		if err == nil || !strings.Contains(err.Error(), "same workload") {
			t.Fatalf("bestEffort=%v: wanted collision error, got %v", bestEffort, err)
		}
	}
}

func TestUnsupportedSettingsFailOrAreDropped(t *testing.T) {
	for _, tc := range []struct{ name, service, path string }{
		{"volume", "volumes: ['./data:/data']", "services.web.volumes"},
		{"published port", "ports: ['8080:80']", "services.web.ports[0].published"},
		{"host ip", "ports: ['127.0.0.1:8080:80']", "services.web.ports[0].host_ip"},
		{"healthcheck", "healthcheck: {test: [CMD, 'true']}", "services.web.healthcheck"},
		{"deploy resources", "deploy: {resources: {limits: {cpus: '1'}}}", "services.web.deploy.resources"},
		{"network aliases", "networks: {backend: {aliases: [db]}}", "services.web.networks.backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "name: unsupported\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    " + tc.service + "\n"
			if strings.Contains(tc.service, "backend") {
				body += "networks:\n  backend: {}\n"
			}
			result, err := render(t, body, false)
			if !slices.Contains(droppedPaths(result, err), tc.path) {
				t.Fatalf("strict mode: wanted %s, got %v", tc.path, err)
			}
			result, err = render(t, body, true)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(droppedPaths(result, nil), tc.path) || len(result.Workloads) != 1 {
				t.Fatalf("best effort: wanted %s dropped, got %v", tc.path, result.Dropped)
			}
		})
	}
}

func TestTopLevelNetworkSettings(t *testing.T) {
	for _, tc := range []struct{ name, config, path string }{
		{"isolated network", "internal: true", "internal"},
		{"custom name", "name: private-network", "name"},
		{"driver", "driver: bridge", "driver"},
		{"driver options", "driver_opts: {mtu: '1400'}", "driver_opts"},
		{"ipam", "ipam: {config: [{subnet: 172.20.0.0/16}]}", "ipam"},
		{"explicit false", "enable_ipv4: false", "enable_ipv4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "name: network-test\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    networks: [backend]\nnetworks:\n  backend:\n    " + tc.config + "\n"
			want := "networks.backend." + tc.path
			result, err := render(t, body, false)
			if result != nil || !slices.Contains(droppedPaths(nil, err), want) {
				t.Fatalf("wanted strict %s error and no result; got %v", want, err)
			}
			result, err = render(t, body, true)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(droppedPaths(result, nil), want) {
				t.Fatalf("missing warning for %s: %v", want, result.Dropped)
			}
			if len(result.Networks) != 1 || result.Networks[0].Metadata.Name != "network-test-backend" {
				t.Fatalf("the network should still be created: %+v", result.Networks)
			}
		})
	}
}

// attachments returns the Datum network each workload attaches to.
func attachments(result *Result) map[string]string {
	attached := map[string]string{}
	for _, w := range result.Workloads {
		attached[w.Metadata.Name] = w.Spec.Template.Spec.NetworkInterfaces[0].Network.Name
	}
	return attached
}

func networkNames(result *Result) []string {
	var names []string
	for _, n := range result.Networks {
		names = append(names, n.Metadata.Name)
	}
	return names
}

func TestImplicitDefaultNetworkIsCreatedPerProject(t *testing.T) {
	result, err := render(t, "name: demo\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n  api:\n    image: docker.io/library/nginx:1.27\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"demo-default"}; !slices.Equal(networkNames(result), want) {
		t.Fatalf("networks %v, want %v", networkNames(result), want)
	}
	for workload, network := range attachments(result) {
		if network != "demo-default" {
			t.Errorf("%s attached to %q", workload, network)
		}
	}
	if len(result.ExistingNetworks) != 0 {
		t.Fatalf("nothing outside the project is used: %v", result.ExistingNetworks)
	}
	if objects := result.Objects(); objects[0].GetKind() != "Network" {
		t.Fatalf("networks must be applied before the workloads that use them: %v", objects)
	}
}

func TestNamedNetworksAreCreatedAndAttached(t *testing.T) {
	result, err := render(t, `name: demo
networks:
  frontend: {}
  backend: {}
  unused: {}
services:
  web:
    image: docker.io/library/nginx:1.27
    networks: [frontend]
  api:
    image: docker.io/library/nginx:1.27
    networks: {backend: {}}
  worker:
    image: docker.io/library/nginx:1.27
    networks: [backend]
`, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"demo-backend", "demo-frontend"}; !slices.Equal(networkNames(result), want) {
		t.Fatalf("networks %v, want %v (a network no service uses is not created)", networkNames(result), want)
	}
	want := map[string]string{"demo-web": "demo-frontend", "demo-api": "demo-backend", "demo-worker": "demo-backend"}
	for workload, network := range want {
		if got := attachments(result)[workload]; got != network {
			t.Errorf("%s attached to %q, want %q", workload, got, network)
		}
	}
	annotations := result.Networks[0].Metadata.Annotations
	if annotations[identity.ProjectAnnotation] != "demo" || annotations[identity.NetworkAnnotation] != "backend" {
		t.Fatalf("network lost its Compose identity: %v", annotations)
	}
}

func TestExternalNetworkIsUsedButNotCreated(t *testing.T) {
	result, err := render(t, "name: demo\nnetworks:\n  shared: {external: true, name: existing-net}\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    networks: [shared]\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Networks) != 0 || !slices.Equal(result.ExistingNetworks, []string{"existing-net"}) {
		t.Fatalf("networks=%v existing=%v", networkNames(result), result.ExistingNetworks)
	}
	if got := attachments(result)["demo-web"]; got != "existing-net" {
		t.Fatalf("attached to %q", got)
	}
}

func TestServiceInSeveralNetworksIsAlwaysAnError(t *testing.T) {
	body := `name: demo
networks: {frontend: {}, backend: {}}
services:
  web:
    image: docker.io/library/nginx:1.27
    networks: [frontend, backend]
  api:
    image: docker.io/library/nginx:1.27
    networks: [frontend, backend]
  db:
    image: docker.io/library/nginx:1.27
    networks: [backend]
`
	for _, bestEffort := range []bool{false, true} {
		result, err := render(t, body, bestEffort)
		var unsupported *UnsupportedError
		if result != nil || err == nil || errors.As(err, &unsupported) {
			t.Fatalf("bestEffort=%v: wanted a hard error, got result=%v err=%v", bestEffort, result, err)
		}
		for _, want := range []string{"services.web is attached to 2 networks (backend, frontend)", "services.api is attached to 2 networks", "one network at a time"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("bestEffort=%v: error lacks %q:\n%v", bestEffort, want, err)
			}
		}
		if strings.Contains(err.Error(), "services.db") {
			t.Errorf("single-network service reported:\n%v", err)
		}
	}
}

func TestNetworkOverrideAttachesEverythingToAnExistingNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yaml")
	body := "name: demo\nnetworks: {frontend: {}}\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    networks: [frontend]\n  api:\n    image: docker.io/library/nginx:1.27\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	project, err := compose.Load(context.Background(), compose.Options{Files: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Location: "us-west-1", RuntimeClass: "general-purpose", Network: "shared-net"}
	if _, err = Translate(project, opts); err == nil || !strings.Contains(err.Error(), "networks.frontend") {
		t.Fatalf("the ignored Compose network must be reported in strict mode: %v", err)
	}
	opts.BestEffort = true
	result, err := Translate(project, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Networks) != 0 || !slices.Equal(result.ExistingNetworks, []string{"shared-net"}) {
		t.Fatalf("networks=%v existing=%v", networkNames(result), result.ExistingNetworks)
	}
	for workload, network := range attachments(result) {
		if network != "shared-net" {
			t.Errorf("%s attached to %q", workload, network)
		}
	}
}

func TestLongNetworkNamesAreHashed(t *testing.T) {
	long := strings.Repeat("n", 70)
	result, err := render(t, "name: demo\nnetworks: {"+long+": {}}\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    networks: ["+long+"]\n", false)
	if err != nil {
		t.Fatal(err)
	}
	name := result.Networks[0].Metadata.Name
	if len(name) > 63 || attachments(result)["demo-web"] != name {
		t.Fatalf("network %q, attachment %q", name, attachments(result)["demo-web"])
	}
	if result.Networks[0].Metadata.Annotations[identity.NetworkAnnotation] != long {
		t.Fatal("lost the original network name")
	}
}

func TestEmptyStartupOverrides(t *testing.T) {
	for _, field := range []string{"command", "entrypoint"} {
		for _, value := range []string{"[]", "''"} {
			t.Run(field+value, func(t *testing.T) {
				body := fmt.Sprintf("name: empty\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    %s: %s\n", field, value)
				if _, err := render(t, body, false); err == nil || !strings.Contains(err.Error(), "services.web."+field) {
					t.Fatalf("wanted empty override rejection: %v", err)
				}
				result, err := render(t, body, true)
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Dropped) != 1 || !strings.Contains(result.Dropped[0].Reason, "image default") {
					t.Fatalf("missing consequence: %v", result.Dropped)
				}
			})
		}
	}
	for _, body := range []string{"command: null\n    entrypoint: null", "command: ['-g', 'daemon off;']\n    entrypoint: [nginx]"} {
		result, err := render(t, "name: startup\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    "+body+"\n", false)
		if err != nil || len(result.Dropped) > 0 {
			t.Fatalf("valid startup override rejected: %v", err)
		}
	}
}

func TestHTTPPortCreatesPublicProxyAndPrivateNetworkService(t *testing.T) {
	result, err := render(t, `name: publish-test
services:
  web:
    image: docker.io/library/nginx:1.27
    http-port: 80
    ports:
      - target: 80
  private:
    image: docker.io/library/nginx:1.27
    ports:
      - target: 8080
`, false)
	if err != nil {
		t.Fatal(err)
	}
	var services []string
	for _, n := range result.NetworkServices {
		services = append(services, n.Metadata.Name)
	}
	if want := []string{"publish-test-private", "publish-test-web"}; !slices.Equal(services, want) {
		t.Fatalf("network services %v, want %v", services, want)
	}
	if len(result.HTTPProxies) != 1 {
		t.Fatalf("wanted one proxy, for the public service only: %+v", result.HTTPProxies)
	}
	proxy := result.HTTPProxies[0]
	backend := proxy.Spec.Rules[1].Backends[0].NetworkService
	if proxy.Metadata.Name != "publish-test-web" || backend.Name != "publish-test-web" || backend.Port != "port-0" {
		t.Fatalf("proxy does not route to the declared port: %+v", proxy)
	}
	if objects := result.Objects(); len(objects) != 6 || objects[0].GetKind() != "Network" || objects[1].GetKind() != "Workload" || objects[5].GetKind() != "HTTPProxy" {
		t.Fatalf("objects are not in dependency order: %v", objects)
	}
}

func TestHTTPPortRequiresMatchingTCPPort(t *testing.T) {
	for _, tc := range []struct{ name, value, ports string }{
		{"missing", "80", ""},
		{"different target", "8080", "    ports: [{target: 80}]\n"},
		{"udp", "80", "    ports: [{target: 80, protocol: udp}]\n"},
		{"out of range", "70000", "    ports: [{target: 80}]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := render(t, "name: invalid\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    http-port: "+tc.value+"\n"+tc.ports, true)
			if err == nil || !strings.Contains(err.Error(), "services.web.http-port") {
				t.Fatalf("invalid public port was accepted: %v", err)
			}
		})
	}
}

func TestUDPPortsStayOffNetworkService(t *testing.T) {
	result, err := render(t, "name: udp\nservices:\n  dns:\n    image: docker.io/library/nginx:1.27\n    ports: [{target: 53, protocol: udp}, {target: 8053}]\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if ports := result.Workloads[0].Spec.Template.Spec.Runtime.Sandbox.Containers[0].Ports; len(ports) != 2 {
		t.Fatalf("container ports: %+v", ports)
	}
	if ports := result.NetworkServices[0].Spec.Ports; len(ports) != 1 || ports[0].Name != "port-1" {
		t.Fatalf("network service ports: %+v", ports)
	}
}
