package filter

import (
	"fmt"
	"regexp"
)

// CompiledFilter holds a compiled regex for matching.
type CompiledFilter struct {
	re      *regexp.Regexp
	pattern string
}

// Compile compiles a single regex pattern into a CompiledFilter.
func Compile(pattern string) (*CompiledFilter, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("filter: invalid pattern %q: %w", pattern, err)
	}
	return &CompiledFilter{re: re, pattern: pattern}, nil
}

// CompileAll compiles a slice of regex patterns, returning an error on the first failure.
func CompileAll(patterns []string) ([]*CompiledFilter, error) {
	filters := make([]*CompiledFilter, 0, len(patterns))
	for _, p := range patterns {
		cf, err := Compile(p)
		if err != nil {
			return nil, err
		}
		filters = append(filters, cf)
	}
	return filters, nil
}
