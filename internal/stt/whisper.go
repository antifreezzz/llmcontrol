package stt

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	BinaryPath string
	ModelPath  string
}

type Service struct {
	mu         sync.Mutex
	binaryPath string
	modelPath  string
}

func NewService(cfg Config) *Service {
	bin := cfg.BinaryPath
	if bin == "" {
		bin = "/home/antifreezzz/whisper.cpp/build-vk/bin/whisper-cli"
	}
	model := cfg.ModelPath
	if model == "" {
		model = "/home/antifreezzz/whisper.cpp/models/ggml-tiny.bin"
	}
	return &Service{
		binaryPath: bin,
		modelPath:  model,
	}
}

func (s *Service) GetConfig() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binaryPath, s.modelPath
}

func (s *Service) UpdateConfig(bin, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bin != "" {
		s.binaryPath = bin
	}
	if model != "" {
		s.modelPath = model
	}
}

func (s *Service) Transcribe(ctx context.Context, audioReader io.Reader, filename string, language string) (string, error) {
	s.mu.Lock()
	bin := s.binaryPath
	model := s.modelPath
	s.mu.Unlock()

	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("whisper-cli binary not found at %s: %w", bin, err)
	}
	if _, err := os.Stat(model); err != nil {
		return "", fmt.Errorf("whisper model not found at %s: %w", model, err)
	}

	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".mp3"
	}

	tmpFile, err := os.CreateTemp("", fmt.Sprintf("stt_*%s", ext))
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, audioReader); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write audio data: %w", err)
	}
	tmpFile.Close()

	if language == "" {
		language = "auto"
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, bin,
		"-m", model,
		"-f", tmpPath,
		"-l", language,
		"-nt",
		"--no-prints",
	)

	outBytes, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("whisper execution error: %w", err)
	}

	cleaned := CleanWhisperOutput(string(outBytes))
	return cleaned, nil
}

func CleanWhisperOutput(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "ggml_") ||
			strings.HasPrefix(trimmed, "read_audio_data:") ||
			strings.HasPrefix(trimmed, "whisper_") ||
			strings.HasPrefix(trimmed, "system_info:") ||
			strings.HasPrefix(trimmed, "main:") {
			continue
		}
		result = append(result, trimmed)
	}
	return strings.Join(result, " ")
}
