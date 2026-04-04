package filter

import (
	"testing"
)

// --- Compile tests ---

func TestCompileValid(t *testing.T) {
	cf, err := Compile(`(?P<ip>\d+\.\d+\.\d+\.\d+)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cf == nil {
		t.Fatal("expected non-nil CompiledFilter")
	}
}

func TestCompileInvalid(t *testing.T) {
	_, err := Compile(`[invalid`)
	if err == nil {
		t.Fatal("expected error for invalid pattern, got nil")
	}
}

// --- Match tests ---

func TestMatchIncludeFirst(t *testing.T) {
	inc, _ := CompileAll([]string{`Failed password.*from (?P<ip>\d+\.\d+\.\d+\.\d+)`})
	line := "Failed password for root from 1.2.3.4 port 22"
	res, err := Match(line, inc, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected Result, got nil")
	}
	if res.IP != "1.2.3.4" {
		t.Errorf("expected IP 1.2.3.4, got %q", res.IP)
	}
	if res.Line != line {
		t.Errorf("expected Line to equal input line")
	}
}

func TestMatchNoMatch(t *testing.T) {
	inc, _ := CompileAll([]string{`NOPE`})
	line := "Failed password for root from 1.2.3.4 port 22"
	res, err := Match(line, inc, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result, got %+v", res)
	}
}

func TestMatchExcludeOverride(t *testing.T) {
	inc, _ := CompileAll([]string{`Failed password.*from (?P<ip>\d+\.\d+\.\d+\.\d+)`})
	exc, _ := CompileAll([]string{`root`})
	line := "Failed password for root from 1.2.3.4 port 22"
	res, err := Match(line, inc, exc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result due to exclude, got %+v", res)
	}
}

func TestMatchExcludeNotTriggered(t *testing.T) {
	inc, _ := CompileAll([]string{`Failed password.*from (?P<ip>\d+\.\d+\.\d+\.\d+)`})
	exc, _ := CompileAll([]string{`admin`})
	line := "Failed password for root from 1.2.3.4 port 22"
	res, err := Match(line, inc, exc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected Result, got nil")
	}
	if res.IP != "1.2.3.4" {
		t.Errorf("expected IP 1.2.3.4, got %q", res.IP)
	}
}

// --- Extract tests ---

func TestExtractNamedGroup(t *testing.T) {
	cf, _ := Compile(`from (?P<ip>\d+\.\d+\.\d+\.\d+)`)
	got := Extract(cf.re, "Failed from 10.0.0.1 port 22")
	if got != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %q", got)
	}
}

func TestExtractFallback(t *testing.T) {
	cf, _ := Compile(`from (\d+\.\d+\.\d+\.\d+)`)
	got := Extract(cf.re, "Failed from 10.0.0.1 port 22")
	if got != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %q", got)
	}
}

// --- ValidateNetType tests ---

func TestValidateNetTypeIP(t *testing.T) {
	if err := ValidateNetType("192.168.1.1", "IP"); err != nil {
		t.Errorf("valid IP returned error: %v", err)
	}
	if err := ValidateNetType("not-an-ip", "IP"); err == nil {
		t.Error("invalid IP should return error")
	}
}

func TestValidateNetTypeCIDR(t *testing.T) {
	if err := ValidateNetType("192.168.1.0/24", "CIDR"); err != nil {
		t.Errorf("valid CIDR returned error: %v", err)
	}
	if err := ValidateNetType("not-a-cidr", "CIDR"); err == nil {
		t.Error("invalid CIDR should return error")
	}
}
