// Package tap drives an OpenVPN TAP-Windows6 (tap0901) virtual Ethernet adapter.
package tap

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	netClassKey = `SYSTEM\CurrentControlSet\Control\Class\{4D36E972-E325-11CE-BFC1-08002BE10318}`
	netConnKey  = `SYSTEM\CurrentControlSet\Control\Network\{4D36E972-E325-11CE-BFC1-08002BE10318}`
)

// Adapter identifies an installed TAP adapter.
type Adapter struct {
	GUID string // including braces, e.g. {1234...}
	Name string // Windows connection name, e.g. "Ethernet 3"
}

// List returns all installed tap0901 adapters.
func List() ([]Adapter, error) {
	class, err := registry.OpenKey(registry.LOCAL_MACHINE, netClassKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, fmt.Errorf("tap: open network class key: %w", err)
	}
	defer class.Close()
	subkeys, err := class.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("tap: enumerate network adapters: %w", err)
	}

	var out []Adapter
	for _, sk := range subkeys {
		k, err := registry.OpenKey(class, sk, registry.QUERY_VALUE)
		if err != nil {
			continue // e.g. "Properties", which is access-restricted
		}
		comp, _, err1 := k.GetStringValue("ComponentId")
		guid, _, err2 := k.GetStringValue("NetCfgInstanceId")
		k.Close()
		if err1 != nil || err2 != nil {
			continue
		}
		comp = strings.ToLower(comp)
		if comp != "tap0901" && comp != `root\tap0901` {
			continue
		}
		out = append(out, Adapter{GUID: guid, Name: connectionName(guid)})
	}
	return out, nil
}

func connectionName(guid string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, netConnKey+`\`+guid+`\Connection`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("Name")
	return name
}

// Find returns the adapter whose connection name or GUID matches want
// (case-insensitive, braces optional). An empty want selects the first
// adapter found.
func Find(want string) (Adapter, error) {
	adapters, err := List()
	if err != nil {
		return Adapter{}, err
	}
	if len(adapters) == 0 {
		return Adapter{}, errors.New("tap: no TAP-Windows6 (tap0901) adapters installed")
	}
	if want == "" {
		return adapters[0], nil
	}
	bare := strings.Trim(want, "{}")
	for _, a := range adapters {
		if strings.EqualFold(a.Name, want) || strings.EqualFold(strings.Trim(a.GUID, "{}"), bare) {
			return a, nil
		}
	}
	return Adapter{}, fmt.Errorf("tap: no tap0901 adapter named or with GUID %q", want)
}
