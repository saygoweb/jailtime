package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJailtimedServiceAllowsRequiredWritableDirectories(t *testing.T) {
	servicePath := filepath.Join("jailtimed.service")

	data, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("read %s: %v", servicePath, err)
	}

	content := string(data)
	requiredDirectives := []string{
		"ProtectSystem=strict",
		"CacheDirectory=whois_cache",
		"LogsDirectory=intrusion",
	}

	for _, directive := range requiredDirectives {
		if !strings.Contains(content, directive) {
			t.Fatalf("service file missing %q", directive)
		}
	}
}
