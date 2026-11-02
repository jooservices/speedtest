// Package netpath validates and applies a node-local network binding for one
// speed-test run. It models no relationships between nodes or uplinks.
package netpath

import (
	"fmt"
	"net"
	"strings"
)

// Binding selects either a local interface or one local source address. An
// empty binding deliberately leaves routing to the operating system.
type Binding struct {
	Interface string
	SourceIP  string
}

// Validate checks values that do not require querying the current host.
func (b Binding) Validate() error {
	b = b.normalized()
	if b.Interface != "" && b.SourceIP != "" {
		return fmt.Errorf("set only one of network_interface or source_ip")
	}
	if len(b.Interface) > 15 {
		return fmt.Errorf("network_interface %q exceeds Linux's 15-character interface-name limit", b.Interface)
	}
	if b.SourceIP != "" && net.ParseIP(b.SourceIP) == nil {
		return fmt.Errorf("source_ip %q must be an IPv4 or IPv6 literal", b.SourceIP)
	}
	return nil
}

func (b Binding) normalized() Binding {
	b.Interface = strings.TrimSpace(b.Interface)
	b.SourceIP = strings.TrimSpace(b.SourceIP)
	return b
}

// Path is the host-validated binding used by an individual run.
type Path struct {
	Interface string
	SourceIP  net.IP
}

// Description is suitable for the agent's status log.
func (p Path) Description() string {
	switch {
	case p.Interface != "":
		return "interface " + p.Interface
	case p.SourceIP != nil:
		return "source IP " + p.SourceIP.String()
	default:
		return "default route"
	}
}

// Resolve verifies that a selected interface or source address currently
// exists and is assigned to an up interface. It should be called at startup
// and again immediately before each measurement.
func Resolve(binding Binding) (Path, error) {
	return resolve(binding, systemInterfaces{})
}

type interfaceCatalog interface {
	byName(string) (net.Interface, error)
	all() ([]net.Interface, error)
	addresses(net.Interface) ([]net.Addr, error)
}

type systemInterfaces struct{}

func (systemInterfaces) byName(name string) (net.Interface, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return net.Interface{}, err
	}
	return *iface, nil
}

func (systemInterfaces) all() ([]net.Interface, error) {
	return net.Interfaces()
}

func (systemInterfaces) addresses(iface net.Interface) ([]net.Addr, error) {
	return iface.Addrs()
}

func resolve(binding Binding, interfaces interfaceCatalog) (Path, error) {
	binding = binding.normalized()
	if err := binding.Validate(); err != nil {
		return Path{}, err
	}
	if binding.Interface == "" && binding.SourceIP == "" {
		return Path{}, nil
	}
	if binding.Interface != "" {
		iface, err := interfaces.byName(binding.Interface)
		if err != nil {
			return Path{}, fmt.Errorf("network_interface %q does not exist", binding.Interface)
		}
		if iface.Flags&net.FlagUp == 0 {
			return Path{}, fmt.Errorf("network_interface %q is down", binding.Interface)
		}
		addresses, err := interfaces.addresses(iface)
		if err != nil {
			return Path{}, fmt.Errorf("read addresses for network_interface %q: %w", binding.Interface, err)
		}
		if !hasUsableAddress(addresses) {
			return Path{}, fmt.Errorf("network_interface %q has no IP address", binding.Interface)
		}
		return Path{Interface: iface.Name}, nil
	}

	want := net.ParseIP(binding.SourceIP)
	all, err := interfaces.all()
	if err != nil {
		return Path{}, fmt.Errorf("list network interfaces: %w", err)
	}
	for _, iface := range all {
		addresses, err := interfaces.addresses(iface)
		if err != nil {
			continue
		}
		if !containsIP(addresses, want) {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			return Path{}, fmt.Errorf("source_ip %q is assigned to down interface %q", want, iface.Name)
		}
		return Path{SourceIP: want}, nil
	}
	return Path{}, fmt.Errorf("source_ip %q is not assigned to an up local interface", want)
}

func hasUsableAddress(addresses []net.Addr) bool {
	for _, address := range addresses {
		if ip := addressIP(address); usableIP(ip) {
			return true
		}
	}
	return false
}

func containsIP(addresses []net.Addr, want net.IP) bool {
	for _, address := range addresses {
		if ip := addressIP(address); usableIP(ip) && ip.Equal(want) {
			return true
		}
	}
	return false
}

func addressIP(address net.Addr) net.IP {
	if address == nil {
		return nil
	}
	switch value := address.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil {
			return ip
		}
		return nil
	}
}

func usableIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsMulticast()
}
