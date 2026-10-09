package identity

import (
	"strings"
	"testing"

	"github.com/dlotterman/datumctl-compose/internal/datum"
)

func TestNamesAreValidAndDistinct(t *testing.T) {
	for _, tc := range []struct{ name, project, first, second string }{
		{"punctuation", "demo", "api-web", "api_web"},
		{"case", "demo", "Web", "web"},
		{"truncation", strings.Repeat("p", 45), strings.Repeat("s", 25) + "a", strings.Repeat("s", 25) + "b"},
		{"long identities", strings.Repeat("p", 80), strings.Repeat("s", 80) + "a", strings.Repeat("s", 80) + "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := WorkloadName(tc.project, tc.first), WorkloadName(tc.project, tc.second)
			if first == second {
				t.Fatalf("both services map to %s", first)
			}
			for _, name := range []string{first, second, ContainerName(tc.first), ContainerName(tc.second)} {
				if !isDNSLabel(name) {
					t.Errorf("invalid name %q", name)
				}
			}
			for _, value := range []string{LabelValue(tc.project), LabelValue(tc.first), LabelValue(tc.second)} {
				if len(value) > maxNameLength || !validLabelValue.MatchString(value) {
					t.Errorf("invalid label value %q", value)
				}
			}
		})
	}
}

func TestPlainNamesAreUnchanged(t *testing.T) {
	// The example deployment depends on these names staying the same.
	if got := WorkloadName("nginx-postgres", "nginx-one"); got != "nginx-postgres-nginx-one" {
		t.Fatalf("WorkloadName = %s", got)
	}
}

func TestAmbiguousProjectBoundariesCannotAdoptEachOther(t *testing.T) {
	if WorkloadName("a-b", "c") != WorkloadName("a", "b-c") {
		t.Fatal("fixture must exercise a name collision between two projects")
	}
	live := Meta(WorkloadName("a-b", "c"), "a-b", "c")
	if OwnsService(live, "a", "b-c") {
		t.Fatal("adopted another project's same-name workload")
	}
}

func TestHashedLabelsRequireFullOriginalIdentity(t *testing.T) {
	project := strings.Repeat("p", 70)
	m := datum.ObjectMeta{Labels: map[string]string{ManagedByLabel: ManagerName, ProjectLabel: LabelValue(project)}}
	if OwnsProject(m, project) {
		t.Fatal("hashed label alone must not establish ownership")
	}
	m.Annotations = map[string]string{ProjectAnnotation: project}
	if !OwnsProject(m, project) {
		t.Fatal("matching full identity was rejected")
	}
	m.Annotations[ProjectAnnotation] = "another-project"
	if OwnsProject(m, project) {
		t.Fatal("conflicting original identity was accepted")
	}
}

func TestMetaOwnsItself(t *testing.T) {
	project, service := strings.Repeat("P", 70), "Web_Server"
	meta := Meta(WorkloadName(project, service), project, service)
	if !OwnsService(meta, project, service) || OwnsService(meta, project, "other") {
		t.Fatalf("Meta and OwnsService disagree: %+v", meta)
	}
}

func TestNetworkMetaOwnsItselfAndNothingElse(t *testing.T) {
	project := strings.Repeat("P", 70)
	meta := NetworkMeta(NetworkName(project, "backend"), project, "backend")
	if !OwnsNetwork(meta, project, "backend") || OwnsNetwork(meta, project, "frontend") || OwnsNetwork(meta, "other", "backend") {
		t.Fatalf("NetworkMeta and OwnsNetwork disagree: %+v", meta)
	}
	if OwnsService(meta, project, "backend") {
		t.Fatal("a network must not be mistaken for a service")
	}
	meta.Annotations = nil
	if OwnsNetwork(meta, project, "backend") {
		t.Fatal("labels alone must not establish network ownership")
	}
}

func TestLabelsWithoutAnnotationsDoNotEstablishOwnership(t *testing.T) {
	live := Meta("demo-web", "demo", "web")
	live.Annotations = nil
	if OwnsProject(live, "demo") || OwnsService(live, "demo", "web") {
		t.Fatal("a resource with matching labels but no annotations was accepted")
	}
}
