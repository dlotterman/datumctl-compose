package translate

import (
	"encoding/json"
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"
)

// supportedServiceFields are the Compose service keys that this package
// translates or that compose-go has already applied (env_file, profiles).
// Any other key with a non-empty value is reported as an issue, so a new
// Compose feature is never silently ignored.
var supportedServiceFields = map[string]bool{
	"image":       true,
	"environment": true,
	"env_file":    true,
	"entrypoint":  true,
	"command":     true,
	"ports":       true,
	"scale":       true,
	"deploy":      true, // only deploy.replicas; checked below
	"networks":    true, // attachment checked by planNetworks; options checked below
	"profiles":    true,
}

// unsupportedServiceFields reports every service setting outside
// supportedServiceFields, plus the unsupported parts of the allowed ones.
// Port settings are checked by containerPorts.
func unsupportedServiceFields(name string, service types.ServiceConfig) []Issue {
	var issues []Issue
	add := func(path, reason string) {
		issues = append(issues, Issue{"services." + name + "." + path, reason})
	}

	// Checking the service's JSON form instead of its Go fields means every
	// key compose-go knows about is covered, including ones added later.
	for key, value := range setFields(service) {
		// compose-go encodes some booleans and numbers without omitempty,
		// so false and 0 count as unset here.
		if supportedServiceFields[key] || string(value) == "false" || string(value) == "0" {
			continue
		}
		add(key, "no Datum mapping")
	}
	if service.Deploy != nil {
		for key := range setFields(service.Deploy) {
			if key != "replicas" {
				add("deploy."+key, "no Datum mapping")
			}
		}
	}

	// nil means "use the image default"; an empty list means "clear it".
	if service.Command != nil && len(service.Command) == 0 {
		add("command", "Datum cannot reliably clear the image CMD; dropping this override keeps the image default")
	}
	if service.Entrypoint != nil && len(service.Entrypoint) == 0 {
		add("entrypoint", "Datum cannot reliably clear the image ENTRYPOINT; dropping this override keeps the image default")
	}

	// Which networks a service may join is checked by planNetworks.
	for network, config := range service.Networks {
		if config != nil && len(setFields(config)) > 0 {
			add("networks."+network, "network options such as aliases have no Datum mapping")
		}
	}
	return issues
}

// setFields returns the fields of v's JSON encoding whose values are not
// null, "", [] or {}.
func setFields(v any) map[string]json.RawMessage {
	encoded, err := json.Marshal(v)
	if err != nil {
		// compose-go's types always marshal; failing here is a programming error.
		panic(fmt.Sprintf("encoding %T: %v", v, err))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		panic(fmt.Sprintf("decoding %T: %v", v, err))
	}
	for key, value := range fields {
		switch string(value) {
		case "null", `""`, "[]", "{}":
			delete(fields, key)
		}
	}
	return fields
}
