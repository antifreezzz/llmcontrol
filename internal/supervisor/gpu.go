package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

type gpuMetrics struct {
	name    string
	totalMB int
	usedMB  int
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

	// 2. Query GPU: Cascade NVIDIA -> AMD -> Intel / DRM -> Fallback
	var gm *gpuMetrics

	// A. Check NVIDIA (nvidia-smi)
	if gm == nil {
		gm = queryNvidiaGPU()
	}

	// B. Check AMD (sysfs)
	if gm == nil {
		gm = queryAmdGPU()
	}

	// C. Check Intel / Generic Linux DRM (sysfs + fdinfo)
	if gm == nil {
		gm = queryIntelDrmGPU()
	}

	if gm != nil && gm.totalMB > 0 {
		stat.GPUName = gm.name
		stat.VRAMTotalMB = gm.totalMB
		stat.VRAMUsedMB = gm.usedMB
		stat.VRAMFreeMB = gm.totalMB - gm.usedMB
		if stat.VRAMFreeMB < 0 {
			stat.VRAMFreeMB = 0
		}
		stat.VRAMUsagePct = float64(stat.VRAMUsedMB) / float64(stat.VRAMTotalMB) * 100
	}

	return stat
}

// queryNvidiaGPU attempts to fetch GPU name & memory via nvidia-smi with a short timeout.
func queryNvidiaGPU() *gpuMetrics {
	_, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.total,memory.used", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil
	}

	// Take the first GPU (or aggregate if multi-GPU in the future)
	parts := strings.Split(lines[0], ",")
	if len(parts) < 3 {
		return nil
	}

	name := strings.TrimSpace(parts[0])
	totalMB, err1 := strconv.Atoi(strings.TrimSpace(parts[1]))
	usedMB, err2 := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err1 != nil || err2 != nil || totalMB <= 0 {
		return nil
	}

	return &gpuMetrics{
		name:    name,
		totalMB: totalMB,
		usedMB:  usedMB,
	}
}

// queryAmdGPU checks /sys/class/drm/card*/device for AMDGPU VRAM sysfs nodes.
func queryAmdGPU() *gpuMetrics {
	cardMatches, _ := filepath.Glob("/sys/class/drm/card[0-9]")
	for _, card := range cardMatches {
		totPath := filepath.Join(card, "device", "mem_info_vram_total")
		usedPath := filepath.Join(card, "device", "mem_info_vram_used")

		totBytes, err1 := readInt64FromFile(totPath)
		usedBytes, err2 := readInt64FromFile(usedPath)
		if err1 == nil && err2 == nil && totBytes > 0 {
			// Find GPU name
			name := "AMD Radeon GPU"
			if prodName, err := os.ReadFile(filepath.Join(card, "device", "product_name")); err == nil {
				cleanName := strings.TrimSpace(string(prodName))
				if cleanName != "" {
					name = cleanName
				}
			} else if lspciName := detectGPUNameFromPCI(card); lspciName != "" {
				name = lspciName
			}

			return &gpuMetrics{
				name:    name,
				totalMB: int(totBytes / (1024 * 1024)),
				usedMB:  int(usedBytes / (1024 * 1024)),
			}
		}
	}
	return nil
}

// queryIntelDrmGPU detects Intel Arc / Iris / DRM GPUs and parses VRAM from PCI resource & fdinfo.
func queryIntelDrmGPU() *gpuMetrics {
	totalVRAMBytes := int64(0)
	var activeCard string

	cardMatches, _ := filepath.Glob("/sys/class/drm/card[0-9]")
	for _, card := range cardMatches {
		// First try Xe driver sysfs if available
		totPath := filepath.Join(card, "device", "mem_info_vram_total")
		if totBytes, err := readInt64FromFile(totPath); err == nil && totBytes > 0 {
			totalVRAMBytes = totBytes
			activeCard = card
			break
		}

		// Fallback: parse PCI BAR memory resource
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
								activeCard = card
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

	if totalVRAMBytes <= 0 {
		return nil
	}

	// Read Used VRAM from /proc/*/fdinfo/*
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
						bytesVal := ParseMemUnit(valStr)
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

	// Dynamically detect GPU name
	name := detectIntelOrDrmGPUName(activeCard)

	return &gpuMetrics{
		name:    name,
		totalMB: int(totalVRAMBytes / (1024 * 1024)),
		usedMB:  int(usedVRAMBytes / (1024 * 1024)),
	}
}

func detectIntelOrDrmGPUName(cardPath string) string {
	if cardPath != "" {
		if lspciName := detectGPUNameFromPCI(cardPath); lspciName != "" {
			return lspciName
		}

		// Read PCI device ID from sysfs uevent / device
		if ueventBytes, err := os.ReadFile(filepath.Join(cardPath, "device", "uevent")); err == nil {
			content := string(ueventBytes)
			for _, line := range strings.Split(content, "\n") {
				if strings.HasPrefix(line, "PCI_ID=") {
					pciID := strings.TrimPrefix(line, "PCI_ID=")
					parts := strings.Split(pciID, ":")
					if len(parts) == 2 {
						vendor := strings.ToLower(parts[0])
						dev := strings.ToLower(parts[1])
						if vendor == "8086" {
							switch dev {
							case "56a0":
								return "Intel Arc A770"
							case "56a1":
								return "Intel Arc A750"
							case "56a2":
								return "Intel Arc A580"
							case "56a5":
								return "Intel Arc A380"
							case "56a6":
								return "Intel Arc A310"
							case "56b0":
								return "Intel Arc Pro A60"
							case "56b1":
								return "Intel Arc Pro A50"
							case "56b2":
								return "Intel Arc Pro A40"
							default:
								return fmt.Sprintf("Intel Arc GPU (Device %s)", dev)
							}
						}
					}
				}
			}
		}
	}

	return "Intel / DRM GPU"
}

func detectGPUNameFromPCI(cardPath string) string {
	// Try finding PCI slot from sysfs
	slotBytes, err := os.ReadFile(filepath.Join(cardPath, "device", "uevent"))
	if err != nil {
		return ""
	}

	var pciSlot string
	for _, line := range strings.Split(string(slotBytes), "\n") {
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			pciSlot = strings.TrimPrefix(line, "PCI_SLOT_NAME=")
			break
		}
	}

	if pciSlot == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lspci", "-s", pciSlot)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	line := strings.TrimSpace(string(out))
	if idx := strings.Index(line, ": "); idx != -1 {
		res := line[idx+2:]
		// If starts with "VGA compatible controller: " or "3D controller: "
		if cIdx := strings.Index(res, ": "); cIdx != -1 {
			res = res[cIdx+2:]
		}
		// Strip revision if present
		if rIdx := strings.Index(res, " (rev "); rIdx != -1 {
			res = res[:rIdx]
		}
		return strings.TrimSpace(res)
	}

	return ""
}

func readInt64FromFile(path string) (int64, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(bytes)), 10, 64)
}

func ParseMemUnit(s string) int64 {
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
		gpuName := s.GPUName
		if gpuName == "" {
			gpuName = "GPU"
		}
		res += fmt.Sprintf("\n🎮 VRAM (%s): %d / %d MB (%.1f%%)", gpuName, s.VRAMUsedMB, s.VRAMTotalMB, s.VRAMUsagePct)
	}
	return res
}

