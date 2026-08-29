package supervisor

import (
	"strings"
	"testing"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

func TestBuildDiffusionArgs(t *testing.T) {
	sup, _, _ := setupTestSupervisor(t)

	model := &db.Model{
		ID:        "z-image-turbo",
		Name:      "Z-Image-Turbo",
		EngineID:  "sd-vk",
		ModelType: "diffusion",
		ModelPath: "/models/z-image-turbo-Q4_K_M.gguf",
		VAEPath:   "/models/ae.safetensors",
		CLIPPath:  "/models/qwen_3_4b.safetensors",
	}

	profile := &db.Profile{
		ModelID:        "z-image-turbo",
		Name:           "default",
		ClipOnCPU:      true,
		VAEOnCPU:       false,
		OffloadParams:  false,
		Width:          1024,
		Height:         1024,
		Steps:          4,
		CFGScale:       1.0,
		SamplingMethod: "euler",
	}

	req := ImageGenRequest{
		ModelID:     "z-image-turbo",
		ProfileName: "default",
		Prompt:      "a gorgeous sunset over snowy mountains",
	}

	args := sup.BuildDiffusionArgs(model, profile, req, "/tmp/out.png", 12345)
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "--mode img_gen") {
		t.Errorf("missing --mode img_gen: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--diffusion-model /models/z-image-turbo-Q4_K_M.gguf") {
		t.Errorf("missing --diffusion-model: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--vae /models/ae.safetensors") {
		t.Errorf("missing --vae: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--llm /models/qwen_3_4b.safetensors") {
		t.Errorf("missing --llm for Qwen encoder: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--prompt a gorgeous sunset over snowy mountains") {
		t.Errorf("missing prompt: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--width 1024") {
		t.Errorf("missing width 1024: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--height 1024") {
		t.Errorf("missing height 1024: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--steps 4") {
		t.Errorf("missing steps 4: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--cfg-scale 1.00") {
		t.Errorf("missing cfg-scale 1.00: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--seed 12345") {
		t.Errorf("missing seed: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--clip-on-cpu") {
		t.Errorf("missing --clip-on-cpu: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--sampling-method euler") {
		t.Errorf("missing sampling method: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--output /tmp/out.png") {
		t.Errorf("missing output: %s", argsStr)
	}
}

func TestBuildDiffusionArgsOverride(t *testing.T) {
	sup, _, _ := setupTestSupervisor(t)

	model := &db.Model{
		ID:        "sdxl-base",
		Name:      "SDXL Base",
		EngineID:  "sd-vk",
		ModelType: "diffusion",
		ModelPath: "/models/sdxl.gguf",
		CLIPPath:  "/models/clip_l.safetensors",
	}

	clipFalse := false
	vaeTrue := true
	offloadTrue := true
	req := ImageGenRequest{
		ModelID:        "sdxl-base",
		Prompt:         "cyberpunk city",
		NegativePrompt: "blurry",
		Width:          768,
		Height:         768,
		Steps:          25,
		CFGScale:       6.5,
		ClipOnCPU:      &clipFalse,
		VAEOnCPU:       &vaeTrue,
		OffloadParams:  &offloadTrue,
		ExtraArgs:      "--schedule karras",
	}

	args := sup.BuildDiffusionArgs(model, nil, req, "/tmp/out2.png", 999)
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "--clip_l /models/clip_l.safetensors") {
		t.Errorf("expected --clip_l for non-qwen clip, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--negative-prompt blurry") {
		t.Errorf("missing negative prompt: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--width 768 --height 768") {
		t.Errorf("missing 768x768 dimensions: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--steps 25") {
		t.Errorf("missing steps 25: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--vae-on-cpu") {
		t.Errorf("missing --vae-on-cpu: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--offload-params-to-cpu") {
		t.Errorf("missing --offload-params-to-cpu: %s", argsStr)
	}
	if strings.Contains(argsStr, "--clip-on-cpu") {
		t.Errorf("did not expect --clip-on-cpu when set to false: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--schedule karras") {
		t.Errorf("missing extra args: %s", argsStr)
	}
}
