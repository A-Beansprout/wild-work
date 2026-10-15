package main

import (
	"testing"

	"wild-work/internal/custom"
	"wild-work/internal/provider"
	"wild-work/internal/stats"
)

func TestBuildCustomRuntimes(t *testing.T) {
	stateDir := t.TempDir()
	enabled := true
	disabled := false

	channels := []custom.ChannelConfig{
		{
			Name:    "stepfun",
			BaseURL: "https://api.stepfun.com/step_plan/v1",
			APIKey:  "key-step",
			Models:  []custom.ModelConfig{{ID: "step-5-preview"}},
			Enabled: &enabled,
		},
		{
			Name:    "offchan",
			BaseURL: "http://127.0.0.1:8000/v1",
			Enabled: &disabled,
		},
	}

	sRts, aRts, names := buildCustomRuntimes(channels, stateDir)
	if len(sRts) != 1 || len(aRts) != 1 || len(names) != 1 {
		t.Fatalf("expected 1 enabled custom runtime, got s=%d, a=%d, names=%v", len(sRts), len(aRts), names)
	}

	k := provider.Kind("stepfun")
	sRt := sRts[k]
	if sRt == nil || sRt.Kind != k || !sRt.SingleAccount || !sRt.NoCooldownOnServerError {
		t.Fatalf("server runtime attributes mismatch: %+v", sRt)
	}
	if len(sRt.Pool.List()) != 1 || sRt.Pool.List()[0].UID != "custom-stepfun" {
		t.Fatalf("expected pool with custom-stepfun, got: %+v", sRt.Pool.List())
	}

	aRt := aRts[k]
	if aRt == nil || aRt.Kind != k || !aRt.Custom {
		t.Fatalf("app runtime attributes mismatch: %+v", aRt)
	}

	if !stats.IsKnownChannel("stepfun") {
		t.Errorf("stepfun should be registered in stats channelKind")
	}
}
