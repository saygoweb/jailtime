package filter

// Result is returned from a successful match.
type Result struct {
	IP      string // extracted IP or CIDR text
	Line    string // original log line
	Pattern string // matched filter pattern
}

// Match applies include-first + optional exclude semantics:
//  1. Try each includeFilter in order; take the first match.
//  2. If no include filter matches, return nil, nil (line ignored).
//  3. Try each excludeFilter; if any matches, return nil, nil (suppressed).
//  4. Extract the named capture group "ip" from the include match.
//  5. Return a Result with IP, Line, Pattern.
func Match(line string, includes, excludes []*CompiledFilter) (*Result, error) {
	// Step 1 & 2: find first matching include filter.
	var matched *CompiledFilter
	for _, f := range includes {
		if f.re.MatchString(line) {
			matched = f
			break
		}
	}
	if matched == nil {
		return nil, nil
	}

	// Step 3: check exclude filters.
	for _, f := range excludes {
		if f.re.MatchString(line) {
			return nil, nil
		}
	}

	// Step 4: extract IP from named (or first) capture group.
	ip := Extract(matched.re, line)

	// Step 5: return result.
	return &Result{
		IP:      ip,
		Line:    line,
		Pattern: matched.pattern,
	}, nil
}
