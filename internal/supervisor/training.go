package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

type TrainingRequest struct {
	Name           string  `json:"name"`
	BaseModelPath  string  `json:"base_model_path"`
	DatasetPath    string  `json:"dataset_path"`
	ValDatasetPath string  `json:"val_dataset_path,omitempty"`
	OutputName     string  `json:"output_name"`
	Epochs         int     `json:"epochs,omitempty"`
	BatchSize      int     `json:"batch_size,omitempty"`
	GradAccum      int     `json:"grad_accum,omitempty"`
	LearningRate   float64 `json:"learning_rate,omitempty"`
	LoraR          int     `json:"lora_r,omitempty"`
	LoraAlpha      int     `json:"lora_alpha,omitempty"`
	TargetModules  string  `json:"target_modules,omitempty"`
	QuantType      string  `json:"quant_type,omitempty"` // "q8_0", "q4_k_m", "fp16", "none"
}

func (s *Supervisor) GetActiveTrainingJob() *db.TrainingJob {
	s.activeTrainMu.RLock()
	defer s.activeTrainMu.RUnlock()
	if s.activeTrainJob == nil {
		return nil
	}
	job := *s.activeTrainJob
	return &job
}

func (s *Supervisor) CancelTraining() bool {
	s.activeTrainMu.Lock()
	defer s.activeTrainMu.Unlock()
	if s.activeTrainCancel != nil {
		s.activeTrainCancel()
		s.activeTrainCancel = nil
		if s.activeTrainJob != nil {
			s.activeTrainJob.Status = "cancelled"
			_ = s.db.UpdateTrainingJobStatus(context.Background(), s.activeTrainJob.ID, "cancelled", "", "Cancelled by user")
		}
		s.broadcastEvent("training:cancelled")
		return true
	}
	return false
}

func (s *Supervisor) StartTraining(ctx context.Context, req TrainingRequest) (*db.TrainingJob, error) {
	s.activeTrainMu.Lock()
	if s.activeTrainJob != nil && s.activeTrainJob.Status == "running" {
		s.activeTrainMu.Unlock()
		return nil, fmt.Errorf("a training job is already running: %s", s.activeTrainJob.ID)
	}

	// 1. Exclusive mode: Stop all active inference engines to free up VRAM
	if s.exclusiveMode {
		_ = s.StopAll(ctx)
		s.CancelImageGeneration()
	}

	// 2. Default parameters
	if req.Epochs <= 0 {
		req.Epochs = 3
	}
	if req.BatchSize <= 0 {
		req.BatchSize = 2
	}
	if req.GradAccum <= 0 {
		req.GradAccum = 8
	}
	if req.LearningRate <= 0 {
		req.LearningRate = 0.0002
	}
	if req.LoraR <= 0 {
		req.LoraR = 16
	}
	if req.LoraAlpha <= 0 {
		req.LoraAlpha = 32
	}
	if req.TargetModules == "" {
		req.TargetModules = "all-linear"
	}
	if req.QuantType == "" {
		req.QuantType = "q8_0"
	}
	if req.OutputName == "" {
		req.OutputName = fmt.Sprintf("finetuned-%s-%d", filepath.Base(req.BaseModelPath), time.Now().Unix())
	}

	// 3. Generate Job ID
	bytes := make([]byte, 6)
	_, _ = rand.Read(bytes)
	jobID := "train-" + hex.EncodeToString(bytes)

	now := time.Now()
	job := db.TrainingJob{
		ID:             jobID,
		Name:           req.Name,
		BaseModelPath:  req.BaseModelPath,
		DatasetPath:    req.DatasetPath,
		ValDatasetPath: req.ValDatasetPath,
		OutputName:     req.OutputName,
		Epochs:         req.Epochs,
		BatchSize:      req.BatchSize,
		GradAccum:      req.GradAccum,
		LearningRate:   req.LearningRate,
		LoraR:          req.LoraR,
		LoraAlpha:      req.LoraAlpha,
		TargetModules:  req.TargetModules,
		QuantType:      req.QuantType,
		Status:         "running",
		CurrentStep:    0,
		TotalSteps:     0,
		CreatedAt:      now,
		StartedAt:      &now,
	}

	if err := s.db.CreateTrainingJob(ctx, job); err != nil {
		s.activeTrainMu.Unlock()
		return nil, fmt.Errorf("failed to create training job in db: %w", err)
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	s.activeTrainJob = &job
	s.activeTrainCancel = cancel
	s.activeTrainMu.Unlock()

	s.broadcastEvent(fmt.Sprintf("training:started:%s", jobID))

	// 4. Run worker in background
	go s.runTrainingWorker(jobCtx, job, req)

	return &job, nil
}

func (s *Supervisor) runTrainingWorker(ctx context.Context, job db.TrainingJob, req TrainingRequest) {
	defer func() {
		s.activeTrainMu.Lock()
		s.activeTrainCancel = nil
		s.activeTrainMu.Unlock()
	}()

	home, _ := os.UserHomeDir()
	logFile := filepath.Join(s.logDir, fmt.Sprintf("%s.log", job.ID))
	logF, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logF != nil {
		defer logF.Close()
	}

	// Locate Python runner
	pythonBin := "python3"
	possibleVenvs := []string{
		filepath.Join(home, "Documents/projects/uporka-training/.venv/bin/python"),
		filepath.Join(home, ".llmcontrol/training/.venv/bin/python"),
	}
	for _, p := range possibleVenvs {
		if _, err := os.Stat(p); err == nil {
			pythonBin = p
			break
		}
	}

	// Locate training script
	trainerScript := filepath.Join(home, "Documents/projects/uporka-training/train.py")
	if _, err := os.Stat(trainerScript); err != nil {
		// Fallback
		errMsg := "trainer script not found at ~/Documents/projects/uporka-training/train.py"
		_ = s.db.UpdateTrainingJobStatus(context.Background(), job.ID, "failed", "", errMsg)
		s.broadcastEvent(fmt.Sprintf("training:failed:%s:%s", job.ID, errMsg))
		return
	}

	outputDir := filepath.Join(home, ".llmcontrol/training/output", job.ID)
	_ = os.MkdirAll(outputDir, 0755)

	cmd := exec.CommandContext(ctx, pythonBin, trainerScript)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("BASE_MODEL=%s", req.BaseModelPath),
		fmt.Sprintf("DATA_DIR=%s", filepath.Dir(req.DatasetPath)),
		fmt.Sprintf("OUTPUT_DIR=%s", outputDir),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = s.db.UpdateTrainingJobStatus(context.Background(), job.ID, "failed", "", err.Error())
		s.broadcastEvent(fmt.Sprintf("training:failed:%s:%s", job.ID, err.Error()))
		return
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		_ = s.db.UpdateTrainingJobStatus(context.Background(), job.ID, "failed", "", err.Error())
		s.broadcastEvent(fmt.Sprintf("training:failed:%s:%s", job.ID, err.Error()))
		return
	}

	stepReg := regexp.MustCompile(`(\d+)/(\d+)\s+\[.*(?:(\d+\.\d+)s/it|(\d+\.\d+)it/s)\]`)
	lossReg := regexp.MustCompile(`'loss':\s*'([0-9.]+)'`)
	epochReg := regexp.MustCompile(`'epoch':\s*'([0-9.]+)'`)
	accReg := regexp.MustCompile(`'mean_token_accuracy':\s*'([0-9.]+)'`)

	go func() {
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
		for scanner.Scan() {
			line := scanner.Text()
			if logF != nil {
				_, _ = logF.WriteString(line + "\n")
			}

			// Parse step metrics
			if matches := stepReg.FindStringSubmatch(line); len(matches) > 2 {
				step, _ := strconv.Atoi(matches[1])
				total, _ := strconv.Atoi(matches[2])

				s.activeTrainMu.Lock()
				if s.activeTrainJob != nil {
					s.activeTrainJob.CurrentStep = step
					s.activeTrainJob.TotalSteps = total
					if lMatches := lossReg.FindStringSubmatch(line); len(lMatches) > 1 {
						loss, _ := strconv.ParseFloat(lMatches[1], 64)
						s.activeTrainJob.CurrentLoss = loss
					}
					if eMatches := epochReg.FindStringSubmatch(line); len(eMatches) > 1 {
						ep, _ := strconv.ParseFloat(eMatches[1], 64)
						s.activeTrainJob.CurrentEpoch = ep
					}
					if aMatches := accReg.FindStringSubmatch(line); len(aMatches) > 1 {
						acc, _ := strconv.ParseFloat(aMatches[1], 64)
						s.activeTrainJob.Accuracy = acc
					}

					_ = s.db.UpdateTrainingJobProgress(
						context.Background(), job.ID,
						s.activeTrainJob.CurrentStep, s.activeTrainJob.TotalSteps,
						s.activeTrainJob.CurrentEpoch, s.activeTrainJob.CurrentLoss,
						s.activeTrainJob.ValLoss, s.activeTrainJob.Accuracy,
					)

					progressJSON, _ := json.Marshal(s.activeTrainJob)
					s.broadcastEvent(fmt.Sprintf("training:progress:%s", string(progressJSON)))
				}
				s.activeTrainMu.Unlock()
			}
		}
	}()

	err = cmd.Wait()
	if err != nil {
		if ctx.Err() == context.Canceled {
			_ = s.db.UpdateTrainingJobStatus(context.Background(), job.ID, "cancelled", "", "Cancelled by user")
			s.broadcastEvent(fmt.Sprintf("training:cancelled:%s", job.ID))
		} else {
			_ = s.db.UpdateTrainingJobStatus(context.Background(), job.ID, "failed", "", err.Error())
			s.broadcastEvent(fmt.Sprintf("training:failed:%s:%s", job.ID, err.Error()))
		}
		return
	}

	// 5. Training successfully completed -> export to GGUF
	finalGGUFPath := filepath.Join("/mnt/storage/ai-models/uporka", fmt.Sprintf("%s.gguf", req.OutputName))
	_ = s.db.UpdateTrainingJobStatus(context.Background(), job.ID, "completed", finalGGUFPath, "")

	s.activeTrainMu.Lock()
	if s.activeTrainJob != nil {
		s.activeTrainJob.Status = "completed"
		s.activeTrainJob.OutputGGUFPath = finalGGUFPath
	}
	s.activeTrainMu.Unlock()

	s.broadcastEvent(fmt.Sprintf("training:completed:%s", job.ID))
}
