// Package datum holds the subset of the Datum Cloud API that datumctl-compose
// writes, and a client that runs datumctl to read and change it.
//
// The types mirror the YAML/JSON that datumctl accepts. They contain only the
// fields this plugin sets; they are not a complete model of the Datum API.
package datum

const (
	KindNetwork        = "Network"
	KindWorkload       = "Workload"
	KindNetworkService = "NetworkService"
	KindHTTPProxy      = "HTTPProxy"

	computeAPIVersion    = "compute.datumapis.com/v1alpha"
	networkingAPIVersion = "networking.datumapis.com/v1alpha"
)

// Object is any Datum resource this plugin creates.
type Object interface {
	GetKind() string
	GetMetadata() *ObjectMeta
}

type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

func (t TypeMeta) GetKind() string { return t.Kind }

type ObjectMeta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// Network is a private IPv6 network that workload instances attach to. An
// instance attaches to exactly one network.
type Network struct {
	TypeMeta
	Metadata ObjectMeta  `json:"metadata"`
	Spec     NetworkSpec `json:"spec"`
}

func NewNetwork(meta ObjectMeta) *Network {
	return &Network{
		TypeMeta{networkingAPIVersion, KindNetwork},
		meta,
		NetworkSpec{IPAM: NetworkIPAM{Mode: "Auto"}},
	}
}

func (n *Network) GetMetadata() *ObjectMeta { return &n.Metadata }

type NetworkSpec struct {
	IPAM NetworkIPAM `json:"ipam"`
}

type NetworkIPAM struct {
	// Mode "Auto" has Datum choose the network's address range.
	Mode string `json:"mode"`
}

// Workload runs a set of identical container instances in one or more locations.
type Workload struct {
	TypeMeta
	Metadata ObjectMeta   `json:"metadata"`
	Spec     WorkloadSpec `json:"spec"`
}

func NewWorkload(meta ObjectMeta, spec WorkloadSpec) *Workload {
	return &Workload{TypeMeta{computeAPIVersion, KindWorkload}, meta, spec}
}

func (w *Workload) GetMetadata() *ObjectMeta { return &w.Metadata }

type WorkloadSpec struct {
	Template   InstanceTemplate `json:"template"`
	Placements []Placement      `json:"placements"`
}

type InstanceTemplate struct {
	Spec InstanceSpec `json:"spec"`
}

type InstanceSpec struct {
	Runtime           Runtime            `json:"runtime"`
	NetworkInterfaces []NetworkInterface `json:"networkInterfaces"`
}

type Runtime struct {
	Class     string           `json:"class"`
	Resources RuntimeResources `json:"resources"`
	Sandbox   Sandbox          `json:"sandbox"`
}

type RuntimeResources struct {
	InstanceType string `json:"instanceType,omitempty"`
}

type Sandbox struct {
	Containers []Container `json:"containers"`
}

type Container struct {
	Name            string          `json:"name"`
	Image           string          `json:"image"`
	Command         []string        `json:"command,omitempty"`
	Args            []string        `json:"args,omitempty"`
	Env             []EnvVar        `json:"env,omitempty"`
	Ports           []NamedPort     `json:"ports,omitempty"`
	SecurityContext SecurityContext `json:"securityContext"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NamedPort is used both for container ports and NetworkService ports. The
// name is how an HTTPProxy backend refers to a NetworkService port.
type NamedPort struct {
	Name     string `json:"name"`
	Port     uint32 `json:"port"`
	Protocol string `json:"protocol"`
}

type SecurityContext struct {
	AllowPrivilegeEscalation bool           `json:"allowPrivilegeEscalation"`
	Capabilities             Capabilities   `json:"capabilities"`
	SeccompProfile           SeccompProfile `json:"seccompProfile"`
}

type Capabilities struct {
	Add  []string `json:"add"`
	Drop []string `json:"drop"`
}

type SeccompProfile struct {
	Type string `json:"type"`
}

type NetworkInterface struct {
	Name    string `json:"name"`
	Network Ref    `json:"network"`
}

type Placement struct {
	Name          string        `json:"name"`
	Locations     []Ref         `json:"locations"`
	ScaleSettings ScaleSettings `json:"scaleSettings"`
}

type ScaleSettings struct {
	MinReplicas int `json:"minReplicas"`
}

// Ref refers to another resource by name.
type Ref struct {
	Name string `json:"name"`
}

// NetworkService gives a stable private address to the instances of a workload.
type NetworkService struct {
	TypeMeta
	Metadata ObjectMeta         `json:"metadata"`
	Spec     NetworkServiceSpec `json:"spec"`
}

func NewNetworkService(meta ObjectMeta, spec NetworkServiceSpec) *NetworkService {
	return &NetworkService{TypeMeta{networkingAPIVersion, KindNetworkService}, meta, spec}
}

func (n *NetworkService) GetMetadata() *ObjectMeta { return &n.Metadata }

type NetworkServiceSpec struct {
	NetworkInterfaces InterfaceSelector `json:"networkInterfaces"`
	Ports             []NamedPort       `json:"ports"`
}

type InterfaceSelector struct {
	Selector LabelSelector `json:"selector"`
}

type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// HTTPProxy publishes a NetworkService port on the internet through Datum's
// application load balancer.
type HTTPProxy struct {
	TypeMeta
	Metadata ObjectMeta    `json:"metadata"`
	Spec     HTTPProxySpec `json:"spec"`
}

func NewHTTPProxy(meta ObjectMeta, spec HTTPProxySpec) *HTTPProxy {
	return &HTTPProxy{TypeMeta{networkingAPIVersion, KindHTTPProxy}, meta, spec}
}

func (h *HTTPProxy) GetMetadata() *ObjectMeta { return &h.Metadata }

type HTTPProxySpec struct {
	Rules []HTTPProxyRule `json:"rules"`
}

type HTTPProxyRule struct {
	Matches  []HTTPRouteMatch   `json:"matches"`
	Filters  []HTTPRouteFilter  `json:"filters,omitempty"`
	Backends []HTTPProxyBackend `json:"backends,omitempty"`
}

type HTTPRouteMatch struct {
	Headers []HTTPHeaderMatch `json:"headers,omitempty"`
	Path    *HTTPPathMatch    `json:"path,omitempty"`
}

type HTTPHeaderMatch struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type HTTPPathMatch struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type HTTPRouteFilter struct {
	Type            string               `json:"type"`
	RequestRedirect *HTTPRequestRedirect `json:"requestRedirect,omitempty"`
}

type HTTPRequestRedirect struct {
	Scheme     string `json:"scheme"`
	StatusCode int    `json:"statusCode"`
}

type HTTPProxyBackend struct {
	NetworkService NetworkServicePortRef `json:"networkService"`
}

type NetworkServicePortRef struct {
	Name string `json:"name"`
	Port string `json:"port"`
}
