package supervisor

import (
	"strings"
	"testing"
)

func TestParseMemUnit(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"1024 B", 1024},
		{"1024 KiB", 1024 * 1024},
		{"1024 kB", 1024 * 1024},
		{"256 MiB", 256 * 1024 * 1024},
		{"256 MB", 256 * 1024 * 1024},
		{"16 GiB", 16 * 1024 * 1024 * 1024},
		{"16 GB", 16 * 1024 * 1024 * 1024},
		{"invalid", 0},
		{"", 0},
	}

	for _, tc := range tests {
		got := ParseMemUnit(tc.input)
		if got != tc.expected {
			t.Errorf("ParseMemUnit(%q) = %d; want %d", tc.input, got, tc.expected)
		}
	}
}

func TestSystemStatusFormatSummary(t *testing.T) {
	// 1. Without GPU
	st1 := SystemStatus{
		RAMTotalMB:  32000,
		RAMUsedMB:   8000,
		RAMUsagePct: 25.0,
	}
	summary1 := st1.FormatSummary()
	if !strings.Contains(summary1, "RAM: 8000 / 32000 MB") {
		t.Errorf("expected RAM in summary, got: %s", summary1)
	}
	if strings.Contains(summary1, "VRAM") {
		t.Errorf("should not contain VRAM when VRAMTotalMB is 0, got: %s", summary1)
	}

	// 2. With GPU
	st2 := SystemStatus{
		RAMTotalMB:   32000,
		RAMUsedMB:    8000,
		RAMUsagePct:  25.0,
		GPUName:      "NVIDIA GeForce RTX 4090",
		VRAMTotalMB:  24576,
		VRAMUsedMB:   12288,
		VRAMUsagePct: 50.0,
	}
	summary2 := st2.FormatSummary()
	if !strings.Contains(summary2, "VRAM (NVIDIA GeForce RTX 4090): 12288 / 24576 MB") {
		t.Errorf("expected VRAM in summary, got: %s", summary2)
	}
}

func TestGetSystemStatusExecution(t *testing.T) {
	// Should not panic on any platform
	stat := GetSystemStatus()
	if stat.RAMTotalMB > 0 {
		if stat.RAMUsedMB > stat.RAMTotalMB {
			t.Errorf("RAMUsedMB (%d) > RAMTotalMB (%d)", stat.RAMUsedMB, stat.RAMTotalMB)
		}
	}
}
