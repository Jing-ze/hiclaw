package config

import "testing"

func TestLoadConfigAppliesManagerSpec(t *testing.T) {
	t.Setenv("HICLAW_MANAGER_SPEC", `{
		"model":"qwen-max",
		"runtime":"copaw",
		"image":"hiclaw/manager:test",
		"resources":{
			"requests":{"cpu":"750m","memory":"1536Mi"},
			"limits":{"cpu":"3","memory":"5Gi"}
		}
	}`)
	t.Setenv("HICLAW_DEFAULT_MODEL", "qwen-default")

	cfg := LoadConfig()

	if cfg.ManagerModel != "qwen-max" {
		t.Fatalf("ManagerModel = %q, want %q", cfg.ManagerModel, "qwen-max")
	}
	if cfg.ManagerRuntime != "copaw" {
		t.Fatalf("ManagerRuntime = %q, want %q", cfg.ManagerRuntime, "copaw")
	}
	if cfg.ManagerImage != "hiclaw/manager:test" {
		t.Fatalf("ManagerImage = %q, want %q", cfg.ManagerImage, "hiclaw/manager:test")
	}
	if cfg.K8sManagerCPURequest != "750m" {
		t.Fatalf("K8sManagerCPURequest = %q, want %q", cfg.K8sManagerCPURequest, "750m")
	}
	if cfg.K8sManagerMemoryRequest != "1536Mi" {
		t.Fatalf("K8sManagerMemoryRequest = %q, want %q", cfg.K8sManagerMemoryRequest, "1536Mi")
	}
	if cfg.K8sManagerCPU != "3" {
		t.Fatalf("K8sManagerCPU = %q, want %q", cfg.K8sManagerCPU, "3")
	}
	if cfg.K8sManagerMemory != "5Gi" {
		t.Fatalf("K8sManagerMemory = %q, want %q", cfg.K8sManagerMemory, "5Gi")
	}
}

func TestLoadConfigUsesLegacyManagerEnvFallback(t *testing.T) {
	t.Setenv("HICLAW_MANAGER_MODEL", "legacy-model")
	t.Setenv("HICLAW_MANAGER_RUNTIME", "openclaw")
	t.Setenv("HICLAW_MANAGER_IMAGE", "hiclaw/manager:legacy")
	t.Setenv("HICLAW_K8S_MANAGER_CPU", "4")
	t.Setenv("HICLAW_K8S_MANAGER_MEMORY", "6Gi")

	cfg := LoadConfig()

	if cfg.ManagerModel != "legacy-model" {
		t.Fatalf("ManagerModel = %q, want %q", cfg.ManagerModel, "legacy-model")
	}
	if cfg.ManagerRuntime != "openclaw" {
		t.Fatalf("ManagerRuntime = %q, want %q", cfg.ManagerRuntime, "openclaw")
	}
	if cfg.ManagerImage != "hiclaw/manager:legacy" {
		t.Fatalf("ManagerImage = %q, want %q", cfg.ManagerImage, "hiclaw/manager:legacy")
	}
	if cfg.K8sManagerCPU != "4" {
		t.Fatalf("K8sManagerCPU = %q, want %q", cfg.K8sManagerCPU, "4")
	}
	if cfg.K8sManagerMemory != "6Gi" {
		t.Fatalf("K8sManagerMemory = %q, want %q", cfg.K8sManagerMemory, "6Gi")
	}
}

func TestLoadConfigPanicsOnInvalidManagerSpec(t *testing.T) {
	t.Setenv("HICLAW_MANAGER_SPEC", "{")

	defer func() {
		if recover() == nil {
			t.Fatal("LoadConfig() did not panic on invalid HICLAW_MANAGER_SPEC")
		}
	}()

	_ = LoadConfig()
}
