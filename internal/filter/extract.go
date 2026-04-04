package filter

import "regexp"

// Extract attempts to get the "ip" named capture group from a regex match on line.
// Falls back to the first unnamed capture group if no named "ip" group exists.
// Returns "" if no capture is found.
func Extract(re *regexp.Regexp, line string) string {
	match := re.FindStringSubmatch(line)
	if match == nil {
		return ""
	}

	// Try named "ip" group first.
	for i, name := range re.SubexpNames() {
		if name == "ip" && i < len(match) {
			return match[i]
		}
	}

	// Fall back to first unnamed capture group (index 1).
	if len(match) > 1 {
		return match[1]
	}
	return ""
}
