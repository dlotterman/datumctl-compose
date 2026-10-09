package compose

import (
	"gopkg.in/yaml.v3"
)

// HTTPPortExtension is where the service's http-port value ends up after
// loading. Read it from types.ServiceConfig.Extensions.
const HTTPPortExtension = "x-datum-http-port"

const httpPortKey = "http-port"

// rewriteHTTPPort renames every services.<name>.http-port key in one Compose
// file to x-datum-http-port. compose-go accepts x-* keys as extensions, so
// interpolation, file merging, and profiles then work on http-port like on
// any other field.
//
// It also reports, per service, whether http-port was set to null. Files
// without http-port are returned unchanged.
func rewriteHTTPPort(content []byte) (rewritten []byte, isNull map[string]bool, err error) {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, nil, err
	}
	isNull = map[string]bool{}
	if len(document.Content) == 0 {
		return content, isNull, nil
	}
	services := mappingValue(document.Content[0], "services")
	if services == nil {
		return content, isNull, nil
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, service := services.Content[i].Value, services.Content[i+1]
		if service.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(service.Content); j += 2 {
			key, value := service.Content[j], service.Content[j+1]
			if key.Value == httpPortKey {
				key.Value = HTTPPortExtension
				isNull[name] = value.Tag == "!!null"
			}
		}
	}
	if len(isNull) == 0 {
		// Re-encoding would change formatting, and with it the line
		// numbers in compose-go's error messages.
		return content, isNull, nil
	}
	rewritten, err = yaml.Marshal(&document)
	return rewritten, isNull, err
}

// mappingValue returns the mapping stored under key in a YAML mapping node,
// or nil when there is none.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			if value := mapping.Content[i+1]; value.Kind == yaml.MappingNode {
				return value
			}
		}
	}
	return nil
}
