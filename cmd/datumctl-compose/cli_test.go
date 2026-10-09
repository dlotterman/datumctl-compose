package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dlotterman/datumctl-compose/internal/datum"
	"github.com/dlotterman/datumctl-compose/internal/identity"
)

// These tests execute a freshly built plugin, including its real os/exec path.
// Every datumctl call goes to a scripted local executable: no account, network,
// authenticated volume, or prebuilt workspace binary is used.
type cliReply struct {
	stdout, stderr string
	code           int
}

type cliCall struct {
	args  []string
	input []byte
}

type cliHarness struct {
	binary, dir, fake string
}

const fakeDatumctlScript = `#!/bin/sh
set -eu
d="$DATUMCTL_COMPOSE_TEST_DIR"
n=0
if [ -f "$d/count" ]; then read -r n < "$d/count"; fi
n=$((n + 1))
printf '%s\n' "$n" > "$d/count"
printf '%s\000' "$@" > "$d/$n.args"
cat > "$d/$n.stdin"
if [ ! -f "$d/$n.status" ]; then
  printf 'unexpected datumctl invocation %s\n' "$n" >&2
  exit 90
fi
cat "$d/$n.stdout"
cat "$d/$n.stderr" >&2
read -r status < "$d/$n.status"
exit "$status"
`

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// liveResource is the subset of a datumctl get -o json response the plugin reads.
type liveResource struct {
	Metadata datum.ObjectMeta `json:"metadata"`
}

// ownedRecord is a live service resource that datumctl-compose created, as
// the API server returns it.
func ownedRecord(name, service string) liveResource {
	meta := identity.Meta(name, "deploy-test", service)
	meta.UID, meta.ResourceVersion = "uid-"+service, "123"
	return liveResource{meta}
}

func jsonRecord(t *testing.T, record liveResource) []byte {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newCLIHarness(t *testing.T, binary, compose string, replies ...cliReply) cliHarness {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "datumctl")
	if err := os.WriteFile(fake, []byte(fakeDatumctlScript), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, "compose.yaml", compose)
	for i, reply := range replies {
		writeFixture(t, dir, fmt.Sprintf("%d.stdout", i+1), reply.stdout)
		writeFixture(t, dir, fmt.Sprintf("%d.stderr", i+1), reply.stderr)
		writeFixture(t, dir, fmt.Sprintf("%d.status", i+1), fmt.Sprintf("%d\n", reply.code))
	}
	return cliHarness{binary, dir, fake}
}

func (h cliHarness) run(t *testing.T, args ...string) (string, string, int, []cliCall) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.binary, args...)
	cmd.Dir = h.dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "COMPOSE_") && !strings.HasPrefix(key, "DATUM_") && key != "DATUMCTL_BIN" && key != "POSTGRES_PASSWORD" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "DATUMCTL_BIN="+h.fake, "DATUMCTL_COMPOSE_TEST_DIR="+h.dir, "POSTGRES_PASSWORD=only-a-test-fixture")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("plugin command timed out: %v", args)
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	var calls []cliCall
	for i := 1; ; i++ {
		argv, err := os.ReadFile(filepath.Join(h.dir, fmt.Sprintf("%d.args", i)))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		input, err := os.ReadFile(filepath.Join(h.dir, fmt.Sprintf("%d.stdin", i)))
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, cliCall{strings.Split(string(bytes.TrimSuffix(argv, []byte{0})), "\x00"), input})
	}
	return stdout.String(), stderr.String(), code, calls
}

func requireCLIResult(t *testing.T, stdout, stderr string, code, wantCode int) {
	t.Helper()
	if code != wantCode {
		t.Fatalf("exit=%d, want %d\nstdout: %s\nstderr: %s", code, wantCode, stdout, stderr)
	}
}

func decodeManifest(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CLI integration fixture requires a POSIX shell")
	}
	binary := filepath.Join(t.TempDir(), "datumctl-compose")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	const compose = "name: deploy-test\nservices:\n  one:\n    image: docker.io/library/nginx:1.27\n  two:\n    image: docker.io/library/nginx:1.27\n"
	const selector = "app.kubernetes.io/managed-by=datumctl-compose,app.kubernetes.io/instance=deploy-test"
	owned := ownedRecord("deploy-test-one", "one")
	list, err := json.Marshal(map[string]any{"items": []liveResource{owned, ownedRecord("deploy-test-two", "two")}})
	if err != nil {
		t.Fatal(err)
	}
	emptyList := cliReply{stdout: `{"items":[]}`}

	t.Run("plugin manifest without configuration or credentials", func(t *testing.T) {
		h := newCLIHarness(t, binary, "invalid yaml: [")
		out, stderr, code, calls := h.run(t, "--plugin-manifest")
		requireCLIResult(t, out, stderr, code, 0)
		var manifest struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			APIVersion int    `json:"api_version"`
		}
		if err := json.Unmarshal([]byte(out), &manifest); err != nil || manifest.Name != "compose" || manifest.Version == "" || manifest.APIVersion != 1 || stderr != "" || len(calls) != 0 {
			t.Fatalf("invalid plugin protocol response: %s / %s / calls=%v / err=%v", out, stderr, calls, err)
		}
	})

	t.Run("example renders workloads and private services offline", func(t *testing.T) {
		example, err := os.ReadFile("../../examples/nginx-postgres.compose.yaml")
		if err != nil {
			t.Fatal(err)
		}
		h := newCLIHarness(t, binary, string(example))
		out, stderr, code, calls := h.run(t, "config", "--location", "us-west-1")
		requireCLIResult(t, out, stderr, code, 0)
		decoder := yaml.NewDecoder(strings.NewReader(out))
		var networks, workloads, services []string
		for {
			var doc map[string]any
			err := decoder.Decode(&doc)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			name := doc["metadata"].(map[string]any)["name"].(string)
			switch doc["kind"] {
			case "Network":
				networks = append(networks, name)
			case "Workload":
				workloads = append(workloads, name)
			case "NetworkService":
				services = append(services, name)
			default:
				t.Fatalf("unexpected public resource: %v", doc)
			}
		}
		want := []string{"nginx-postgres-nginx-one", "nginx-postgres-nginx-two", "nginx-postgres-postgres"}
		if !reflect.DeepEqual(networks, []string{"nginx-postgres-default"}) || !reflect.DeepEqual(workloads, want) || !reflect.DeepEqual(services, want) || stderr != "" || len(calls) != 0 || !strings.Contains(out, "only-a-test-fixture") {
			t.Fatalf("networks=%v workloads=%v services=%v stderr=%s calls=%v", networks, workloads, services, stderr, calls)
		}
	})

	t.Run("strict validation fails before contacting datumctl", func(t *testing.T) {
		h := newCLIHarness(t, binary, compose+"    volumes: ['./data:/data']\n")
		out, stderr, code, calls := h.run(t, "up", "--location", "us-west-1")
		requireCLIResult(t, out, stderr, code, 1)
		if out != "" || !strings.Contains(stderr, "services.two.volumes") || len(calls) != 0 {
			t.Fatalf("validation did not stop deployment: out=%s stderr=%s calls=%v", out, stderr, calls)
		}
	})

	t.Run("best effort keeps warnings out of manifest output", func(t *testing.T) {
		h := newCLIHarness(t, binary, "name: demo\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    volumes: ['./data:/data']\n")
		out, stderr, code, calls := h.run(t, "config", "--location", "us-west-1", "--best-effort")
		requireCLIResult(t, out, stderr, code, 0)
		_, workloadYAML, ok := strings.Cut(out, "---\n")
		if !ok {
			t.Fatalf("wanted a network and a workload document: %s", out)
		}
		doc := decodeManifest(t, []byte(workloadYAML))
		spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		container := spec["runtime"].(map[string]any)["sandbox"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		if spec["volumes"] != nil || container["volumeAttachments"] != nil || !strings.Contains(stderr, "dropping services.web.volumes") || len(calls) != 0 {
			t.Fatalf("bad best-effort output: %s / %s", out, stderr)
		}
	})

	t.Run("up sends stdin manifests and forwards project scope", func(t *testing.T) {
		h := newCLIHarness(t, binary, compose, cliReply{}, cliReply{}, cliReply{stdout: string(jsonRecord(t, ownedRecord("deploy-test-two", "two")))}, emptyList, emptyList, emptyList, cliReply{}, cliReply{}, cliReply{})
		out, stderr, code, calls := h.run(t, "up", "--location", "us-west-1", "--datum-project", "target-project")
		requireCLIResult(t, out, stderr, code, 0)
		wantArgs := [][]string{
			{"--project", "target-project", "get", "network", "deploy-test-default", "--ignore-not-found", "-o", "json"},
			{"--project", "target-project", "get", "workload", "deploy-test-one", "--ignore-not-found", "-o", "json"},
			{"--project", "target-project", "get", "workload", "deploy-test-two", "--ignore-not-found", "-o", "json"},
			{"--project", "target-project", "get", "httpproxies", "-l", selector, "-o", "json"},
			{"--project", "target-project", "get", "networkservices", "-l", selector, "-o", "json"},
			{"--project", "target-project", "get", "networks", "-l", selector, "-o", "json"},
			{"--project", "target-project", "create", "-f", "-", "--field-manager=datumctl-compose"},
			{"--project", "target-project", "create", "-f", "-", "--field-manager=datumctl-compose"},
			{"--project", "target-project", "apply", "-f", "-", "--server-side", "--field-manager=datumctl-compose", "--force-conflicts"},
		}
		if len(calls) != len(wantArgs) {
			t.Fatalf("calls=%v", calls)
		}
		for i, call := range calls {
			if !reflect.DeepEqual(call.args, wantArgs[i]) {
				t.Fatalf("call %d: %v, want %v", i, call.args, wantArgs[i])
			}
		}
		network := decodeManifest(t, calls[6].input)
		create := decodeManifest(t, calls[7].input)["metadata"].(map[string]any)
		update := decodeManifest(t, calls[8].input)["metadata"].(map[string]any)
		if network["kind"] != "Network" || network["metadata"].(map[string]any)["name"] != "deploy-test-default" {
			t.Fatalf("the network must be created first: %v", network)
		}
		if create["name"] != "deploy-test-one" || create["uid"] != nil || update["uid"] != "uid-two" || update["resourceVersion"] != "123" || !strings.Contains(stderr, "datumctl compose ps") || out != "Created network deploy-test-default\nCreated workload deploy-test-one\nUpdated workload deploy-test-two\n" {
			t.Fatalf("incorrect metadata or result: create=%v update=%v out=%s stderr=%s", create, update, out, stderr)
		}
	})

	t.Run("up refuses a foreign service before creating anything", func(t *testing.T) {
		foreign := ownedRecord("deploy-test-two", "two")
		foreign.Metadata.Labels[identity.ManagedByLabel] = "another-deployer"
		h := newCLIHarness(t, binary, compose, cliReply{}, cliReply{}, cliReply{stdout: string(jsonRecord(t, foreign))})
		out, stderr, code, calls := h.run(t, "up", "--location", "us-west-1")
		requireCLIResult(t, out, stderr, code, 1)
		if out != "" || !strings.Contains(stderr, "refusing to update workload deploy-test-two") || len(calls) != 3 {
			t.Fatalf("ownership failure not reported correctly: %s / %s / %v", out, stderr, calls)
		}
	})

	t.Run("up stops after a failed create", func(t *testing.T) {
		h := newCLIHarness(t, binary, compose, cliReply{}, cliReply{}, cliReply{}, emptyList, emptyList, emptyList, cliReply{stderr: "admission rejected create\n", code: 17})
		out, stderr, code, calls := h.run(t, "up", "--location", "us-west-1")
		requireCLIResult(t, out, stderr, code, 1)
		if out != "" || !strings.Contains(stderr, "admission rejected create") || len(calls) != 7 {
			t.Fatalf("continued after failed create: %s / %s / %v", out, stderr, calls)
		}
	})

	t.Run("verbose traces each datumctl command", func(t *testing.T) {
		h := newCLIHarness(t, binary, compose, cliReply{stdout: "NAME\n"})
		out, stderr, code, _ := h.run(t, "ps", "-v", "--datum-project", "target-project")
		requireCLIResult(t, out, stderr, code, 0)
		want := "+ " + h.fake + " --project target-project get networks,workloads,networkservices,httpproxies -l " + selector + "\n"
		if stderr != want {
			t.Fatalf("trace %q, want %q", stderr, want)
		}
	})

	t.Run("failed datumctl call names the command", func(t *testing.T) {
		h := newCLIHarness(t, binary, compose, cliReply{stderr: "permission denied\n", code: 17})
		out, stderr, code, _ := h.run(t, "ps")
		requireCLIResult(t, out, stderr, code, 1)
		if !strings.Contains(stderr, "permission denied") || !strings.Contains(stderr, "get networks,workloads,networkservices,httpproxies -l "+selector+": exit status 17") {
			t.Fatalf("unhelpful error: %q", stderr)
		}
	})

	t.Run("service in two networks is an alarm, even with best effort", func(t *testing.T) {
		multi := "name: multi\nnetworks: {frontend: {}, backend: {}}\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    networks: [frontend, backend]\n"
		for _, command := range []string{"config", "up"} {
			for _, extra := range [][]string{nil, {"--best-effort"}} {
				h := newCLIHarness(t, binary, multi)
				out, stderr, code, calls := h.run(t, append([]string{command, "--location", "us-west-1"}, extra...)...)
				requireCLIResult(t, out, stderr, code, 1)
				if out != "" || len(calls) != 0 || !strings.Contains(stderr, "services.web is attached to 2 networks (backend, frontend)") || !strings.Contains(stderr, "one network at a time") {
					t.Fatalf("%s %v: out=%q stderr=%q calls=%v", command, extra, out, stderr, calls)
				}
			}
		}
	})

	const external = "name: ext\nnetworks:\n  shared: {external: true, name: existing-net}\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    networks: [shared]\n"
	for _, tc := range []struct{ name, live, want string }{
		{"missing", "", `network "existing-net" does not exist in Datum`},
		{"deleting", `{"metadata":{"name":"existing-net","deletionTimestamp":"2026-10-09T01:25:45Z"}}`, `network "existing-net" is being deleted in Datum`},
	} {
		t.Run("up stops before writing when an external network is "+tc.name, func(t *testing.T) {
			h := newCLIHarness(t, binary, external, cliReply{stdout: tc.live})
			out, stderr, code, calls := h.run(t, "up", "--location", "us-west-1")
			requireCLIResult(t, out, stderr, code, 1)
			wantArgs := []string{"get", "network", "existing-net", "--ignore-not-found", "-o", "json"}
			if out != "" || len(calls) != 1 || !reflect.DeepEqual(calls[0].args, wantArgs) || !strings.Contains(stderr, tc.want) {
				t.Fatalf("out=%q stderr=%q calls=%v", out, stderr, calls)
			}
		})
	}

	t.Run("network flag uses an existing network and creates none", func(t *testing.T) {
		h := newCLIHarness(t, binary, compose, cliReply{stdout: `{"metadata":{"name":"mine"}}`}, cliReply{}, cliReply{}, emptyList, emptyList, emptyList, cliReply{}, cliReply{})
		out, stderr, code, calls := h.run(t, "up", "--location", "us-west-1", "--network", "mine")
		requireCLIResult(t, out, stderr, code, 0)
		if out != "Created workload deploy-test-one\nCreated workload deploy-test-two\n" || len(calls) != 8 {
			t.Fatalf("out=%q calls=%v", out, calls)
		}
		spec := decodeManifest(t, calls[6].input)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		if network := spec["networkInterfaces"].([]any)[0].(map[string]any)["network"].(map[string]any)["name"]; network != "mine" {
			t.Fatalf("attached to %v", network)
		}
	})

	t.Run("ps scopes the query and preserves output", func(t *testing.T) {
		const table = "NAME READY\ndeploy-test-one True\n"
		h := newCLIHarness(t, binary, compose, cliReply{stdout: table})
		out, stderr, code, calls := h.run(t, "ps", "--datum-project", "target-project")
		requireCLIResult(t, out, stderr, code, 0)
		want := []string{"--project", "target-project", "get", "networks,workloads,networkservices,httpproxies", "-l", selector}
		if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, want) || out != table || stderr != "" {
			t.Fatalf("ps result: %s / %s / %v", out, stderr, calls)
		}
	})

	for _, tc := range []struct {
		name    string
		replies []cliReply
		code    int
		calls   int
		message string
	}{
		{"down deletes owned workloads", []cliReply{emptyList, emptyList, {stdout: string(list)}, emptyList, {}, {}}, 0, 6, "Deleted workload deploy-test-one\nDeleted workload deploy-test-two\n"},
		{"down with no workloads", []cliReply{emptyList, emptyList, emptyList, emptyList}, 0, 4, "No resources found for deploy-test\n"},
		{"down propagates a query failure", []cliReply{{stderr: "permission denied\n", code: 17}}, 1, 1, ""},
		{"down rejects a malformed response", []cliReply{{stdout: "not json"}}, 1, 1, ""},
		{"down stops after delete failure", []cliReply{emptyList, emptyList, {stdout: string(list)}, emptyList, {stderr: "delete denied\n", code: 17}}, 1, 5, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIHarness(t, binary, compose, tc.replies...)
			out, stderr, code, calls := h.run(t, "down")
			requireCLIResult(t, out, stderr, code, tc.code)
			if len(calls) != tc.calls || out != tc.message {
				t.Fatalf("out=%q stderr=%q calls=%v", out, stderr, calls)
			}
			for i, kind := range []string{"httpproxies", "networkservices", "workloads", "networks"} {
				if i >= len(calls) {
					break
				}
				if !reflect.DeepEqual(calls[i].args, []string{"get", kind, "-l", selector, "-o", "json"}) {
					t.Fatalf("unscoped down query: %v", calls[i].args)
				}
			}
			for i := 4; i < len(calls); i++ {
				name := []string{"deploy-test-one", "deploy-test-two"}[i-4]
				if !reflect.DeepEqual(calls[i].args, []string{"delete", "workload", name}) {
					t.Fatalf("wrong deletion: %v", calls[i].args)
				}
			}
			if tc.code != 0 && stderr == "" {
				t.Fatal("failure must include a diagnostic")
			}
		})
	}

	for _, mismatch := range []string{"manager", "project", "annotation"} {
		t.Run("down refuses mismatched "+mismatch, func(t *testing.T) {
			item := ownedRecord("deploy-test-one", "one")
			switch mismatch {
			case "manager":
				item.Metadata.Labels[identity.ManagedByLabel] = "someone-else"
			case "project":
				item.Metadata.Labels[identity.ProjectLabel] = "someone-else"
			case "annotation":
				item.Metadata.Annotations = map[string]string{identity.ProjectAnnotation: "someone-else"}
			}
			response, err := json.Marshal(map[string]any{"items": []liveResource{item}})
			if err != nil {
				t.Fatal(err)
			}
			h := newCLIHarness(t, binary, compose, emptyList, emptyList, cliReply{stdout: string(response)})
			out, stderr, code, calls := h.run(t, "down")
			requireCLIResult(t, out, stderr, code, 1)
			if out != "" || !strings.Contains(stderr, "refusing to delete") || len(calls) != 3 {
				t.Fatalf("unsafe down result: %s / %s / %v", out, stderr, calls)
			}
		})
	}
}
