package stt

import (
	"testing"
)

func TestCleanWhisperOutput(t *testing.T) {
	raw := `ggml_vulkan: Found 1 Vulkan devices
system_info: n_threads = 4
main: processing 'audio.mp3'
read_audio_data: reading WAV data
whisper_model_load: loading model
Hello and welcome to this video review!
Today we are going to look at gameplay mechanics.
`
	cleaned := CleanWhisperOutput(raw)
	expected := "Hello and welcome to this video review! Today we are going to look at gameplay mechanics."
	if cleaned != expected {
		t.Fatalf("expected %q, got %q", expected, cleaned)
	}
}

func TestServiceConfig(t *testing.T) {
	svc := NewService(Config{
		BinaryPath: "/bin/test-bin",
		ModelPath:  "/models/test-model.bin",
	})
	bin, model := svc.GetConfig()
	if bin != "/bin/test-bin" || model != "/models/test-model.bin" {
		t.Fatalf("unexpected config: %s, %s", bin, model)
	}

	svc.UpdateConfig("/bin/new-bin", "/models/new-model.bin")
	bin, model = svc.GetConfig()
	if bin != "/bin/new-bin" || model != "/models/new-model.bin" {
		t.Fatalf("unexpected updated config: %s, %s", bin, model)
	}
}
