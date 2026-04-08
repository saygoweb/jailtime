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

// ExtractLabel attempts to get the "label" named capture group from a regex match on line.
// Returns "" if no named "label" group exists or it didn't match.
func ExtractLabel(re *regexp.Regexp, line string) string {
	match := re.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	for i, name := range re.SubexpNames() {
		if name == "label" && i < len(match) {
			return match[i]
		}
	}
	return ""
}
