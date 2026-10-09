package translate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/dlotterman/datumctl-compose/internal/compose"
	"github.com/dlotterman/datumctl-compose/internal/datum"
)

// buildNetworkService gives the workload with the same name a private
// address on ports.
func buildNetworkService(meta datum.ObjectMeta, ports []datum.NamedPort) *datum.NetworkService {
	return datum.NewNetworkService(meta, datum.NetworkServiceSpec{
		NetworkInterfaces: datum.InterfaceSelector{Selector: datum.LabelSelector{
			MatchLabels: map[string]string{"compute.datumapis.com/workload-name": meta.Name},
		}},
		Ports: ports,
	})
}

// buildHTTPProxy publishes port of the NetworkService with the same name.
// Plain HTTP requests are redirected to HTTPS.
func buildHTTPProxy(meta datum.ObjectMeta, port string) *datum.HTTPProxy {
	everything := &datum.HTTPPathMatch{Type: "PathPrefix", Value: "/"}
	redirectToHTTPS := datum.HTTPProxyRule{
		Matches: []datum.HTTPRouteMatch{{
			Headers: []datum.HTTPHeaderMatch{{Name: "x-forwarded-proto", Type: "Exact", Value: "http"}},
			Path:    everything,
		}},
		Filters: []datum.HTTPRouteFilter{{
			Type:            "RequestRedirect",
			RequestRedirect: &datum.HTTPRequestRedirect{Scheme: "https", StatusCode: 301},
		}},
	}
	forward := datum.HTTPProxyRule{
		Matches: []datum.HTTPRouteMatch{{Path: everything}},
		Backends: []datum.HTTPProxyBackend{{
			NetworkService: datum.NetworkServicePortRef{Name: meta.Name, Port: port},
		}},
	}
	return datum.NewHTTPProxy(meta, datum.HTTPProxySpec{Rules: []datum.HTTPProxyRule{redirectToHTTPS, forward}})
}

// httpPort returns the service's http-port, or 0 when it has none. The value
// must be one of the service's TCP target ports. These errors are never
// dropped by --best-effort: guessing would publish the wrong port, or none.
func httpPort(name string, service types.ServiceConfig) (uint32, error) {
	value, ok := service.Extensions[compose.HTTPPortExtension]
	if !ok || value == nil {
		return 0, nil
	}
	var text string
	switch v := value.(type) {
	case int:
		text = strconv.Itoa(v)
	case string: // interpolated values, such as ${APP_PORT}, arrive as strings
		text = v
	default:
		return 0, fmt.Errorf("services.%s.http-port must be an integer target port", name)
	}
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("services.%s.http-port must be between 1 and 65535", name)
	}
	for _, declared := range service.Ports {
		if int(declared.Target) == port && isTCP(declared.Protocol) {
			return uint32(port), nil
		}
	}
	return 0, fmt.Errorf("services.%s.http-port %d must also be a TCP target under ports", name, port)
}

func isTCP(protocol string) bool {
	return protocol == "" || strings.EqualFold(protocol, "tcp")
}

func tcpOnly(ports []datum.NamedPort) []datum.NamedPort {
	var tcp []datum.NamedPort
	for _, port := range ports {
		if port.Protocol == "TCP" {
			tcp = append(tcp, port)
		}
	}
	return tcp
}

// portName returns the name of the first port with the given number.
func portName(ports []datum.NamedPort, number uint32) string {
	for _, port := range ports {
		if port.Port == number {
			return port.Name
		}
	}
	return ""
}
