package filter

import (
	"fmt"
	"net/netip"
)

// ValidateNetType checks the extracted IP/CIDR string against netType ("IP" or "CIDR").
// Returns an error if the string doesn't parse correctly for the given type.
func ValidateNetType(extracted string, netType string) error {
	switch netType {
	case "IP":
		if _, err := netip.ParseAddr(extracted); err != nil {
			return fmt.Errorf("filter: %q is not a valid IP address: %w", extracted, err)
		}
	case "CIDR":
		if _, err := netip.ParsePrefix(extracted); err != nil {
			return fmt.Errorf("filter: %q is not a valid CIDR prefix: %w", extracted, err)
		}
	default:
		return fmt.Errorf("filter: unknown netType %q", netType)
	}
	return nil
}
