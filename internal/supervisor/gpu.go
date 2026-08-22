package supervisor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type SystemStatus struct {
	RAMTotalMB  int     `json:"ram_total_mb"`
	RAMUsedMB   int     `json:"ram_used_mb"`
	RAMFreeMB   int     `json:"ram_free_mb"`
	RAMUsagePct float64 `json:"ram_usage_pct"`

	GPUName     string  `json:"gpu_name,omitempty"`
	VRAMTotalMB int     `json:"vram_total_mb,omitempty"`
	VRAMUsedMB  int     `json:"vram_used_mb,omitempty"`
	VRAMFreeMB  int     `json:"vram_free_mb,omitempty"`
	VRAMUsagePct float64 `json:"vram_usage_pct,omitempty"`
}

func GetSystemStatus() SystemStatus {
	stat := SystemStatus{}

	// Read /proc/meminfo
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		var totalKB, freeKB, availableKB int
		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				key := strings.TrimSuffix(fields[0], ":")
				val, _ := strconv.Atoi(fields[1])
				switch key {
				case "MemTotal":
					totalKB = val
				case "MemFree":
					freeKB = val
				case "MemAvailable":
					availableKB = val
				}
			}
		}
		if totalKB > 0 {
			stat.RAMTotalMB = totalKB / 1024
			usedKB := totalKB - availableKB
			if availableKB == 0 {
				usedKB = totalKB - freeKB
			}
			stat.RAMUsedMB = usedKB / 1024
			stat.RAMFreeMB = stat.RAMTotalMB - stat.RAMUsedMB
			stat.RAMUsagePct = float64(stat.RAMUsedMB) / float64(stat.RAMTotalMB) * 100
		}
	}

	// Read Intel Arc GPU VRAM (sysfs /sys/class/drm/card*/device/drm/card*/lmem_total_bytes)
	cardMatches, _ := filepath.Glob("/sys/class/drm/card[0-9]")
	for _, card := range cardMatches {
		lmemTotalPath := filepath.Join(card, "lmem_total_bytes")
		lmemUsedPath := filepath.Join(card, "lmem_used_bytes")

		if _, err := os.Stat(lmemTotalPath); err == nil {
			stat.GPUName = "Intel Arc GPU"
			if b, err := os.ReadFile(lmemTotalPath); err == nil {
				if totalBytes, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
					stat.VRAMTotalMB = int(totalBytes / (1024 * 1024))
				}
			}
			if b, err := os.ReadFile(lmemUsedPath); err == nil {
				if usedBytes, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
					stat.VRAMUsedMB = int(usedBytes / (1024 * 1024))
				}
			}
			if stat.VRAMTotalMB > 0 {
				stat.VRAMFreeMB = stat.VRAMTotalMB - stat.VRAMUsedMB
				stat.VRAMUsagePct = float64(stat.VRAMUsedMB) / float64(stat.VRAMTotalMB) * 100
			}
			break
		}
	}

	return stat
}

func (s SystemStatus) FormatSummary() string {
	res := fmt.Sprintf("🧠 RAM: %d / %d MB (%.1f%%)", s.RAMUsedMB, s.RAMTotalMB, s.RAMUsagePct)
	if s.VRAMTotalMB > 0 {
		res += fmt.Sprintf("\n🎮 VRAM (%s): %d / %d MB (%.1f%%)", s.GPUName, s.VRAMUsedMB, s.VRAMTotalMB, s.VRAMUsagePct)
	}
	return res
}
