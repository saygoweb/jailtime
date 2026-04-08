package filter

import (
	"fmt"
	"net/netip"
)

// NormalizeToCIDR converts a plain IP address to CIDR prefix notation.
// If the input is already a valid CIDR prefix it is returned unchanged.
// A plain IPv4 address gets "/32" appended; IPv6 gets "/128".
// Returns an error if the input is neither a valid IP nor a valid CIDR.
func NormalizeToCIDR(extracted string) (string, error) {
	if _, err := netip.ParsePrefix(extracted); err == nil {
		return extracted, nil
	}
	addr, err := netip.ParseAddr(extracted)
	if err != nil {
		return "", fmt.Errorf("filter: %q is not a valid IP address or CIDR prefix", extracted)
	}
	if addr.Is4() {
		return extracted + "/32", nil
	}
	return extracted + "/128", nil
}

// ValidateNetType checks the extracted IP/CIDR string against netType ("IP" or "CIDR").
// When netType is "CIDR", a plain IP address is also accepted (it will be normalised
// to IP/32 or IP/128 by NormalizeToCIDR before use).
// Returns an error if the string doesn't parse correctly for the given type.
func ValidateNetType(extracted string, netType string) error {
	switch netType {
	case "IP":
		if _, err := netip.ParseAddr(extracted); err != nil {
			return fmt.Errorf("filter: %q is not a valid IP address: %w", extracted, err)
		}
	case "CIDR":
		if _, err := netip.ParsePrefix(extracted); err != nil {
			// Also accept plain IPs; they are normalised to /32 or /128 by the engine.
			if _, addrErr := netip.ParseAddr(extracted); addrErr != nil {
				return fmt.Errorf("filter: %q is not a valid IP address or CIDR prefix: %w", extracted, err)
			}
		}
	default:
		return fmt.Errorf("filter: unknown netType %q", netType)
	}
	return nil
}
