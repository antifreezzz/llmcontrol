package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

// HFModelSibling represents a file in a Hugging Face model repository.
type HFModelSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size,omitempty"`
}

// HFModelInfo represents response from Hugging Face model info API.
type HFModelInfo struct {
	ID        string           `json:"id"`
	Author    string           `json:"author"`
	Siblings  []HFModelSibling `json:"siblings"`
	Downloads int64            `json:"downloads"`
	Likes     int64            `json:"likes"`
}

type HuggingFaceFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SizeStr  string `json:"size_str"`
	Quant    string `json:"quant"`
	IsMMProj bool   `json:"is_mmproj"`
	IsMTP    bool   `json:"is_mtp"`
}

type RepoInfo struct {
	RepoID   string            `json:"repo_id"`
	Files    []HuggingFaceFile `json:"files"`
	HasGGUF  bool              `json:"has_gguf"`
	Likes    int64             `json:"likes"`
	Siblings int               `json:"siblings_count"`
}

type JobStatus string

const (
	StatusPending     JobStatus = "pending"
	StatusDownloading JobStatus = "downloading"
	StatusCompleted   JobStatus = "completed"
	StatusFailed      JobStatus = "failed"
	StatusCancelled   JobStatus = "cancelled"
)

type DownloadJob struct {
	ID               string     `json:"id"`
	RepoID           string     `json:"repo_id"`
	Filename         string     `json:"filename"`
	TargetPath       string     `json:"target_path"`
	TotalBytes       int64      `json:"total_bytes"`
	DownloadedBytes  int64      `json:"downloaded_bytes"`
	SpeedBytesPerSec float64    `json:"speed_bytes_per_sec"`
	SpeedStr         string     `json:"speed_str"`
	ProgressPct      float64    `json:"progress_pct"`
	Status           JobStatus  `json:"status"`
	Error            string     `json:"error,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	AutoRegister     bool       `json:"auto_register"`
	ModelID          string     `json:"model_id,omitempty"`

	cancelFunc context.CancelFunc `json:"-"`
}

type Manager struct {
	httpClient *http.Client
	modelsDir  string
	db         *db.DB
	jobs       map[string]*DownloadJob
	mu         sync.RWMutex
}

func NewManager(modelsDir string, database *db.DB) *Manager {
	if modelsDir == "" {
		home, _ := os.UserHomeDir()
		modelsDir = filepath.Join(home, ".lmstudio", "models")
	}
	return &Manager{
		httpClient: &http.Client{
			Timeout: 0, // No global timeout for long downloads
		},
		modelsDir: modelsDir,
		db:        database,
		jobs:      make(map[string]*DownloadJob),
	}
}

func (m *Manager) GetModelsDir() string {
	return m.modelsDir
}

func FormatBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

var quantRegex = regexp.MustCompile(`(?i)(Q[0-9]+_[A-Z0-9_]+|IQ[0-9]+_[A-Z0-9_]+|BF16|F16|F32|MXFP[0-9]+_[A-Z0-9_]+|UD-Q[0-9]+_[A-Z0-9_]+)`)

func detectQuant(filename string) string {
	match := quantRegex.FindString(filename)
	if match != "" {
		return strings.ToUpper(match)
	}
	return "UNKNOWN"
}

type HFTreeItem struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Size int64 `json:"size"`
	} `json:"lfs,omitempty"`
}

// InspectRepo queries the Hugging Face API to find all GGUF and companion files with accurate sizes.
func (m *Manager) InspectRepo(ctx context.Context, repoID string) (*RepoInfo, error) {
	repoID = strings.TrimSpace(repoID)
	repoID = strings.TrimPrefix(repoID, "https://huggingface.co/")
	repoID = strings.TrimSuffix(repoID, "/")

	// 1. Try tree/main endpoint first for exact file sizes
	treeURL := fmt.Sprintf("https://huggingface.co/api/models/%s/tree/main", repoID)
	req, err := http.NewRequestWithContext(ctx, "GET", treeURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "llmcontrol/1.0")
		if resp, err := m.httpClient.Do(req); err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			var treeItems []HFTreeItem
			if err := json.NewDecoder(resp.Body).Decode(&treeItems); err == nil && len(treeItems) > 0 {
				res := &RepoInfo{
					RepoID:   repoID,
					Files:    make([]HuggingFaceFile, 0),
					Siblings: len(treeItems),
				}
				for _, item := range treeItems {
					if item.Type != "file" {
						continue
					}
					lower := strings.ToLower(item.Path)
					if !strings.HasSuffix(lower, ".gguf") {
						continue
					}
					isMMProj := strings.Contains(lower, "mmproj") || strings.Contains(lower, "vision")
					isMTP := strings.Contains(lower, "mtp") || strings.Contains(lower, "draft") || strings.Contains(lower, "dflash")
					quant := detectQuant(item.Path)

					size := item.Size
					if item.LFS != nil && item.LFS.Size > 0 {
						size = item.LFS.Size
					}

					res.Files = append(res.Files, HuggingFaceFile{
						Filename: item.Path,
						Size:     size,
						SizeStr:  FormatBytes(size),
						Quant:    quant,
						IsMMProj: isMMProj,
						IsMTP:    isMTP,
					})
				}
				if len(res.Files) > 0 {
					res.HasGGUF = true
					sort.Slice(res.Files, func(i, j int) bool {
						a, b := res.Files[i], res.Files[j]
						if (a.IsMMProj || a.IsMTP) != (b.IsMMProj || b.IsMTP) {
							return !(a.IsMMProj || a.IsMTP)
						}
						return a.Filename < b.Filename
					})
					return res, nil
				}
			}
		}
	}

	// 2. Fallback to /api/models/{repoID}
	apiURL := fmt.Sprintf("https://huggingface.co/api/models/%s", repoID)
	req, err = http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "llmcontrol/1.0")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Hugging Face API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Hugging Face API returned status %d (%s)", resp.StatusCode, resp.Status)
	}

	var info HFModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	res := &RepoInfo{
		RepoID:   repoID,
		Files:    make([]HuggingFaceFile, 0),
		Likes:    info.Likes,
		Siblings: len(info.Siblings),
	}

	for _, s := range info.Siblings {
		lower := strings.ToLower(s.RFilename)
		if !strings.HasSuffix(lower, ".gguf") {
			continue
		}

		isMMProj := strings.Contains(lower, "mmproj") || strings.Contains(lower, "vision")
		isMTP := strings.Contains(lower, "mtp") || strings.Contains(lower, "draft") || strings.Contains(lower, "dflash")
		quant := detectQuant(s.RFilename)

		res.Files = append(res.Files, HuggingFaceFile{
			Filename: s.RFilename,
			Size:     s.Size,
			SizeStr:  FormatBytes(s.Size),
			Quant:    quant,
			IsMMProj: isMMProj,
			IsMTP:    isMTP,
		})
	}

	res.HasGGUF = len(res.Files) > 0

	sort.Slice(res.Files, func(i, j int) bool {
		a, b := res.Files[i], res.Files[j]
		if (a.IsMMProj || a.IsMTP) != (b.IsMMProj || b.IsMTP) {
			return !(a.IsMMProj || a.IsMTP)
		}
		return a.Filename < b.Filename
	})

	return res, nil
}

// StartDownload initiates downloading a file from Hugging Face.
func (m *Manager) StartDownload(repoID, filename string, autoRegister bool, modelID string) (*DownloadJob, error) {
	repoID = strings.TrimSpace(repoID)
	repoID = strings.TrimPrefix(repoID, "https://huggingface.co/")
	repoID = strings.TrimSuffix(repoID, "/")
	filename = strings.TrimSpace(filename)

	if repoID == "" || filename == "" {
		return nil, fmt.Errorf("repoID and filename are required")
	}

	jobID := fmt.Sprintf("%s-%d", strings.ReplaceAll(repoID, "/", "-"), time.Now().Unix())

	targetDir := filepath.Join(m.modelsDir, repoID)
	targetPath := filepath.Join(targetDir, filename)

	ctx, cancel := context.WithCancel(context.Background())

	job := &DownloadJob{
		ID:           jobID,
		RepoID:       repoID,
		Filename:     filename,
		TargetPath:   targetPath,
		Status:       StatusPending,
		StartedAt:    time.Now(),
		AutoRegister: autoRegister,
		ModelID:      modelID,
		cancelFunc:   cancel,
	}

	m.mu.Lock()
	m.jobs[jobID] = job
	m.mu.Unlock()

	go m.runDownload(ctx, job)

	return job, nil
}

func (m *Manager) runDownload(ctx context.Context, job *DownloadJob) {
	job.Status = StatusDownloading
	downloadURL := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", job.RepoID, job.Filename)

	// Ensure destination directory
	targetDir := filepath.Dir(job.TargetPath)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to create directory %s: %v", targetDir, err)
		return
	}

	partPath := job.TargetPath + ".part"

	var existingBytes int64 = 0
	if stat, err := os.Stat(partPath); err == nil {
		existingBytes = stat.Size()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to create request: %v", err)
		return
	}
	req.Header.Set("User-Agent", "llmcontrol/1.0")

	if existingBytes > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingBytes))
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			job.Status = StatusCancelled
			job.Error = "download cancelled"
			return
		}
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("download request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("Hugging Face server returned status %d (%s)", resp.StatusCode, resp.Status)
		return
	}

	var totalBytes int64
	if resp.StatusCode == http.StatusPartialContent {
		totalBytes = existingBytes + resp.ContentLength
		job.DownloadedBytes = existingBytes
	} else {
		totalBytes = resp.ContentLength
		existingBytes = 0 // Started from beginning
		job.DownloadedBytes = 0
	}
	job.TotalBytes = totalBytes

	flags := os.O_CREATE | os.O_WRONLY
	if existingBytes > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	file, err := os.OpenFile(partPath, flags, 0644)
	if err != nil {
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to open output file: %v", err)
		return
	}
	defer file.Close()

	buf := make([]byte, 256*1024)
	lastTime := time.Now()
	var bytesSinceLast int64 = 0

	for {
		if ctx.Err() != nil {
			job.Status = StatusCancelled
			job.Error = "download cancelled"
			return
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				job.Status = StatusFailed
				job.Error = fmt.Sprintf("write error: %v", writeErr)
				return
			}
			job.DownloadedBytes += int64(n)
			bytesSinceLast += int64(n)

			now := time.Now()
			elapsed := now.Sub(lastTime).Seconds()
			if elapsed >= 0.5 {
				speed := float64(bytesSinceLast) / elapsed
				job.SpeedBytesPerSec = speed
				job.SpeedStr = fmt.Sprintf("%s/s", FormatBytes(int64(speed)))
				if job.TotalBytes > 0 {
					job.ProgressPct = float64(job.DownloadedBytes) / float64(job.TotalBytes) * 100.0
				}
				lastTime = now
				bytesSinceLast = 0
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			if ctx.Err() != nil {
				job.Status = StatusCancelled
				job.Error = "download cancelled"
				return
			}
			job.Status = StatusFailed
			job.Error = fmt.Sprintf("read error: %v", readErr)
			return
		}
	}

	// Rename .part to targetPath
	_ = file.Close()
	if err := os.Rename(partPath, job.TargetPath); err != nil {
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to rename file: %v", err)
		return
	}

	now := time.Now()
	job.FinishedAt = &now
	job.Status = StatusCompleted
	job.ProgressPct = 100.0
	job.SpeedStr = "0 B/s"

	// Auto-register model in DB if requested
	if job.AutoRegister && m.db != nil {
		m.autoRegisterModel(job)
	}
}

func (m *Manager) autoRegisterModel(job *DownloadJob) {
	// Skip companion files registration as separate main models
	lower := strings.ToLower(job.Filename)
	if strings.Contains(lower, "mmproj") {
		return
	}

	modelID := job.ModelID
	if modelID == "" {
		parts := strings.Split(job.RepoID, "/")
		repoSlug := parts[len(parts)-1]
		repoSlug = strings.TrimSuffix(repoSlug, "-GGUF")
		repoSlug = strings.TrimSuffix(repoSlug, "-gguf")
		modelID = strings.ToLower(strings.ReplaceAll(repoSlug, ".", ""))
	}

	// Check if model already exists
	ctx := context.Background()
	existing, _ := m.db.GetModel(ctx, modelID)
	if existing != nil {
		return
	}

	// Format display name
	parts := strings.Split(job.RepoID, "/")
	displayName := parts[len(parts)-1]
	if quant := detectQuant(job.Filename); quant != "UNKNOWN" {
		displayName = fmt.Sprintf("%s (%s)", displayName, quant)
	}

	model := db.Model{
		ID:             modelID,
		Name:           displayName,
		EngineID:       "llama-vk",
		ModelPath:      job.TargetPath,
		DefaultPort:    8080,
		DefaultProfile: "default",
		IsFavorite:     false,
	}

	// Look for potential mmproj or mtp companions in the same directory
	targetDir := filepath.Dir(job.TargetPath)
	if entries, err := os.ReadDir(targetDir); err == nil {
		for _, e := range entries {
			eName := e.Name()
			eLower := strings.ToLower(eName)
			if !strings.HasSuffix(eLower, ".gguf") {
				continue
			}
			if strings.Contains(eLower, "mmproj") && model.MMProjPath == "" {
				model.MMProjPath = filepath.Join(targetDir, eName)
			}
			if (strings.Contains(eLower, "mtp") || strings.Contains(eLower, "draft") || strings.Contains(eLower, "dflash")) && model.MTPPath == "" {
				model.MTPPath = filepath.Join(targetDir, eName)
			}
		}
	}

	if err := m.db.SaveModel(ctx, model); err != nil {
		return
	}

	// Create default profile
	defaultProfile := db.Profile{
		ModelID:     modelID,
		Name:        "default",
		Description: "Стандартный профиль",
		CtxSize:     32768,
		Parallel:    1,
		KVType:      "q8_0",
		FlashAttn:   "auto",
		UseMTP:      model.MTPPath != "",
		SpecType:    "",
		UseVision:   model.MMProjPath != "",
		EnableUI:    true,
		Tools:       "safe",
		ExtraArgs:   "-ngl 99 -b 2048 -ub 2048 -t 6 -tb 6 --cache-ram 0",
	}
	if model.MTPPath != "" {
		defaultProfile.SpecType = "draft-mtp"
		defaultProfile.DraftNMax = 2
	}
	_ = m.db.SaveProfile(ctx, defaultProfile)
}

func (m *Manager) GetJob(id string) *DownloadJob {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}

func (m *Manager) ListJobs() []*DownloadJob {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*DownloadJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].StartedAt.After(list[j].StartedAt)
	})
	return list
}

func (m *Manager) CancelJob(id string) error {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("job not found: %s", id)
	}
	if job.cancelFunc != nil {
		job.cancelFunc()
	}
	return nil
}
