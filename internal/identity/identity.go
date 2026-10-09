// Package identity names the Datum resources that datumctl-compose manages,
// labels them with the Compose project and service they came from, and decides
// whether an existing resource belongs to a given project and service.
//
// Labels have strict length and character limits, so long or unusual names are
// shortened with a hash. Annotations have no such limits and always keep the
// original Compose names; ownership checks compare both.
package identity

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/dlotterman/datumctl-compose/internal/datum"
)

const (
	// ManagerName is the managed-by label value and the server-side apply
	// field manager for every resource this plugin writes.
	ManagerName = "datumctl-compose"

	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ProjectLabel      = "app.kubernetes.io/instance"
	ServiceLabel      = "app.kubernetes.io/name"
	ProjectAnnotation = "compose.datum.net/project"
	ServiceAnnotation = "compose.datum.net/service"

	// Networks carry NetworkLabel/NetworkAnnotation instead of the service
	// label and annotation.
	NetworkLabel      = "compose.datum.net/network"
	NetworkAnnotation = "compose.datum.net/network"
)

const (
	maxNameLength   = 63
	hashPrefixLimit = 50
)

var (
	invalidNameChars = regexp.MustCompile(`[^a-z0-9-]+`)
	validLabelValue  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.-]*[a-zA-Z0-9])?$`)
)

// Meta returns the metadata for a resource generated from a Compose service.
func Meta(name, project, service string) datum.ObjectMeta {
	return datum.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			ManagedByLabel: ManagerName,
			ProjectLabel:   LabelValue(project),
			ServiceLabel:   LabelValue(service),
		},
		Annotations: map[string]string{
			ProjectAnnotation: project,
			ServiceAnnotation: service,
		},
	}
}

// NetworkMeta returns the metadata for the Datum network created from a
// Compose network.
func NetworkMeta(name, project, network string) datum.ObjectMeta {
	return datum.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			ManagedByLabel: ManagerName,
			ProjectLabel:   LabelValue(project),
			NetworkLabel:   LabelValue(network),
		},
		Annotations: map[string]string{
			ProjectAnnotation: project,
			NetworkAnnotation: network,
		},
	}
}

// Selector is the datumctl label selector for every resource in a project.
func Selector(project string) string {
	return ManagedByLabel + "=" + ManagerName + "," + ProjectLabel + "=" + LabelValue(project)
}

// OwnsProject reports whether meta belongs to the Compose project. Both the
// label and the annotation holding the full project name must match: the label
// may be a hashed form of the name, so on its own it does not prove ownership.
func OwnsProject(meta datum.ObjectMeta, project string) bool {
	if meta.Labels[ManagedByLabel] != ManagerName || meta.Labels[ProjectLabel] != LabelValue(project) {
		return false
	}
	return meta.Annotations[ProjectAnnotation] == project
}

// OwnsService reports whether meta belongs to this service of the project.
func OwnsService(meta datum.ObjectMeta, project, service string) bool {
	if !OwnsProject(meta, project) || meta.Labels[ServiceLabel] != LabelValue(service) {
		return false
	}
	return meta.Annotations[ServiceAnnotation] == service
}

// OwnsNetwork reports whether meta is the Datum network created from this
// Compose network of the project.
func OwnsNetwork(meta datum.ObjectMeta, project, network string) bool {
	if !OwnsProject(meta, project) || meta.Labels[NetworkLabel] != LabelValue(network) {
		return false
	}
	return meta.Annotations[NetworkAnnotation] == network
}

// NetworkName is the name of the Datum network created from a Compose
// network. Like WorkloadName, it is "<project>-<network>" when that is a valid
// DNS label.
func NetworkName(project, network string) string {
	return WorkloadName(project, network)
}

// WorkloadName is the name shared by a service's Workload, NetworkService,
// and HTTPProxy. It is "<project>-<service>" when that is already a valid DNS
// label, and a shortened name with a hash suffix otherwise.
func WorkloadName(project, service string) string {
	plain := project + "-" + service
	if isDNSLabel(plain) {
		return plain
	}
	// Hash the project and service separately so "a-b"+"c" and "a"+"b-c"
	// get different names.
	return hashedName(plain, project+"\x00"+service)
}

// ContainerName is the service name when it is a valid DNS label, and a
// shortened name with a hash suffix otherwise.
func ContainerName(service string) string {
	if isDNSLabel(service) {
		return service
	}
	return hashedName(service, service)
}

// LabelValue is value when it is a valid label value, and a shortened value
// with a hash suffix otherwise.
func LabelValue(value string) string {
	if len(value) <= maxNameLength && validLabelValue.MatchString(value) {
		return value
	}
	return hashedName(value, value)
}

func isDNSLabel(s string) bool {
	return len(s) <= maxNameLength && normalize(s) == s
}

// hashedName returns a DNS label made from a readable prefix and a hash of
// identity. The result is at most 63 characters.
func hashedName(prefix, identity string) string {
	prefix = normalize(prefix)
	if len(prefix) > hashPrefixLimit {
		prefix = prefix[:hashPrefixLimit]
	}
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "compose"
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x", prefix, sum[:6])
}

// normalize lowercases s and replaces every run of characters that are not
// allowed in a DNS label with a single dash.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = invalidNameChars.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
