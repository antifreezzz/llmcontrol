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
	RAMTotalMB   int     `json:"ram_total_mb"`
	RAMUsedMB    int     `json:"ram_used_mb"`
	RAMFreeMB    int     `json:"ram_free_mb"`
	RAMUsagePct  float64 `json:"ram_usage_pct"`

	GPUName      string  `json:"gpu_name,omitempty"`
	VRAMTotalMB  int     `json:"vram_total_mb,omitempty"`
	VRAMUsedMB   int     `json:"vram_used_mb,omitempty"`
	VRAMFreeMB   int     `json:"vram_free_mb,omitempty"`
	VRAMUsagePct float64 `json:"vram_usage_pct,omitempty"`
}

func GetSystemStatus() SystemStatus {
	stat := SystemStatus{}

	// 1. Read /proc/meminfo for System RAM
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

	// 2. Read Intel Arc GPU Total VRAM from PCI resource
	totalVRAMBytes := int64(0)
	cardMatches, _ := filepath.Glob("/sys/class/drm/card[0-9]")
	for _, card := range cardMatches {
		resFile := filepath.Join(card, "device", "resource")
		if f, err := os.Open(resFile); err == nil {
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				cols := strings.Fields(scanner.Text())
				if len(cols) >= 3 {
					start, err1 := strconv.ParseUint(cols[0], 0, 64)
					end, err2 := strconv.ParseUint(cols[1], 0, 64)
					flags, err3 := strconv.ParseUint(cols[2], 0, 64)
					if err1 == nil && err2 == nil && err3 == nil {
						// 0x200 = IORESOURCE_MEM
						if flags&0x200 != 0 && end > start {
							sz := int64(end - start + 1)
							// Check if >= 1 GB
							if sz >= 1024*1024*1024 && sz > totalVRAMBytes {
								totalVRAMBytes = sz
							}
						}
					}
				}
			}
			f.Close()
		}
		if totalVRAMBytes > 0 {
			break
		}
	}

	// 3. Read Intel Arc GPU Used VRAM from /proc/*/fdinfo/*
	usedVRAMBytes := int64(0)
	clientMax := make(map[string]int64)

	procEntries, err := os.ReadDir("/proc")
	if err == nil {
		for _, pe := range procEntries {
			if !pe.IsDir() {
				continue
			}
			pidStr := pe.Name()
			if pidStr[0] < '0' || pidStr[0] > '9' {
				continue
			}

			fdDirPath := filepath.Join("/proc", pidStr, "fdinfo")
			fdEntries, err := os.ReadDir(fdDirPath)
			if err != nil {
				continue
			}

			for _, fde := range fdEntries {
				fdPath := filepath.Join(fdDirPath, fde.Name())
				contentBytes, err := os.ReadFile(fdPath)
				if err != nil {
					continue
				}

				content := string(contentBytes)
				if strings.Contains(content, "drm-resident-local0") || strings.Contains(content, "drm-total-local0") {
					var clientID, valStr string
					lines := strings.Split(content, "\n")
					for _, l := range lines {
						if strings.HasPrefix(l, "drm-client-id:") {
							clientID = strings.TrimSpace(strings.TrimPrefix(l, "drm-client-id:"))
						} else if strings.HasPrefix(l, "drm-resident-local0:") {
							valStr = strings.TrimSpace(strings.TrimPrefix(l, "drm-resident-local0:"))
						} else if strings.HasPrefix(l, "drm-total-local0:") && valStr == "" {
							valStr = strings.TrimSpace(strings.TrimPrefix(l, "drm-total-local0:"))
						}
					}

					if valStr != "" {
						bytesVal := parseMemUnit(valStr)
						key := pidStr + ":" + clientID
						if bytesVal > clientMax[key] {
							clientMax[key] = bytesVal
						}
					}
				}
			}
		}
	}

	for _, b := range clientMax {
		usedVRAMBytes += b
	}

	if totalVRAMBytes > 0 {
		stat.GPUName = "Intel Arc A770 (Vulkan/SYCL)"
		stat.VRAMTotalMB = int(totalVRAMBytes / (1024 * 1024))
		stat.VRAMUsedMB = int(usedVRAMBytes / (1024 * 1024))
		stat.VRAMFreeMB = stat.VRAMTotalMB - stat.VRAMUsedMB
		if stat.VRAMFreeMB < 0 {
			stat.VRAMFreeMB = 0
		}
		stat.VRAMUsagePct = float64(stat.VRAMUsedMB) / float64(stat.VRAMTotalMB) * 100
	}

	return stat
}

func parseMemUnit(s string) int64 {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return 0
	}
	val, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}

	unit := "B"
	if len(parts) > 1 {
		unit = parts[1]
	}

	multiplier := int64(1)
	switch unit {
	case "KiB", "kB", "KB":
		multiplier = 1024
	case "MiB", "MB":
		multiplier = 1024 * 1024
	case "GiB", "GB":
		multiplier = 1024 * 1024 * 1024
	}

	return int64(val * float64(multiplier))
}

func (s SystemStatus) FormatSummary() string {
	res := fmt.Sprintf("🧠 RAM: %d / %d MB (%.1f%%)", s.RAMUsedMB, s.RAMTotalMB, s.RAMUsagePct)
	if s.VRAMTotalMB > 0 {
		res += fmt.Sprintf("\n🎮 VRAM (%s): %d / %d MB (%.1f%%)", s.GPUName, s.VRAMUsedMB, s.VRAMTotalMB, s.VRAMUsagePct)
	}
	return res
}
