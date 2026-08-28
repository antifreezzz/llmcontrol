package downloader

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectQuant(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"model-Q4_K_M.gguf", "Q4_K_M"},
		{"LFM2.5-2.6B-Q8_0.gguf", "Q8_0"},
		{"Ling-3.0-tiny-UD-Q4_K_XL.gguf", "UD-Q4_K_XL"},
		{"Mellum2-12B-A2.5B-Thinking-MXFP4_MOE.gguf", "MXFP4_MOE"},
		{"mmproj-model-BF16.gguf", "BF16"},
		{"other.txt", "UNKNOWN"},
	}

	for _, tt := range tests {
		got := detectQuant(tt.filename)
		if got != tt.expected {
			t.Errorf("detectQuant(%q) = %q, want %q", tt.filename, got, tt.expected)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	if got := FormatBytes(0); got != "0 B" {
		t.Errorf("FormatBytes(0) = %q, want 0 B", got)
	}
	if got := FormatBytes(1024); got != "1.00 KB" {
		t.Errorf("FormatBytes(1024) = %q, want 1.00 KB", got)
	}
	if got := FormatBytes(1024 * 1024 * 500); got != "500.00 MB" {
		t.Errorf("FormatBytes(500MB) = %q, want 500.00 MB", got)
	}
}

func TestDownloadManager(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "llmcontrol-dl-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	content := "GGUF-MOCK-BINARY-CONTENT-FOR-TEST"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "33")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	mgr := NewManager(tmpDir, nil)
	mgr.httpClient = server.Client()

	jobID := "test-job-1"
	targetPath := filepath.Join(tmpDir, "TestOrg", "TestRepo", "test-model-Q4_K_M.gguf")
	job := &DownloadJob{
		ID:         jobID,
		RepoID:     "TestOrg/TestRepo",
		Filename:   "test-model-Q4_K_M.gguf",
		TargetPath: targetPath,
	}
	mgr.jobs[jobID] = job

	retrieved := mgr.GetJob(jobID)
	if retrieved == nil || retrieved.ID != jobID {
		t.Fatalf("expected job %s to be retrieved", jobID)
	}

	list := mgr.ListJobs()
	if len(list) != 1 {
		t.Fatalf("expected 1 job, got %d", len(list))
	}
}
