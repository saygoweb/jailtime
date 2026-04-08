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

// ExtractNamedGroups returns a map of all named capture groups (excluding "ip")
// that matched in line. Groups that are present in the pattern but did not
// participate in the match are included with an empty string value.
// Returns nil when the pattern does not match line at all.
func ExtractNamedGroups(re *regexp.Regexp, line string) map[string]string {
	match := re.FindStringSubmatch(line)
	if match == nil {
		return nil
	}
	groups := make(map[string]string)
	for i, name := range re.SubexpNames() {
		if name != "" && name != "ip" && i < len(match) {
			groups[name] = match[i]
		}
	}
	return groups
}
