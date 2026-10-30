package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SakkijarvenPolka/tahrs-scheduler/internal/model"
)

const packModeConfigTemplate = `
configVersion: test
defaultProfile: cpu
profiles:
  cpu:
    alpha: 0.3
    beta: 0.7
    fitWeights: {cpu: 1, memory: 1}
    packWeights: {cpu: 1, memory: 1}
%s
`

func loadPackMode(t *testing.T, mode string) *Loaded {
	t.Helper()
	line := ""
	if mode != "" {
		line = "    packMode: " + mode
	}
	loaded, err := Load([]byte(fmt.Sprintf(packModeConfigTemplate, line)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return loaded
}

func TestLoadDefaultsPackModeToPack(t *testing.T) {
	loaded := loadPackMode(t, "")
	if got := loaded.Setting.Profiles["cpu"].PackMode; got != PackModePack {
		t.Errorf("packMode = %q, want %q", got, PackModePack)
	}
}

func TestLoadNormalisesPackMode(t *testing.T) {
	loaded := loadPackMode(t, "SPREAD")
	if got := loaded.Setting.Profiles["cpu"].PackMode; got != PackModeSpread {
		t.Errorf("packMode = %q, want %q", got, PackModeSpread)
	}
}

func TestLoadRejectsUnknownPackMode(t *testing.T) {
	_, err := Load([]byte(fmt.Sprintf(packModeConfigTemplate, "    packMode: sideways")))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "packMode") {
		t.Errorf("error should name the field, got %q", err)
	}
}

func TestCompileCarriesPackMode(t *testing.T) {
	loaded := loadPackMode(t, "spread")
	views, err := loaded.Setting.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := views["cpu"].PackMode; got != model.PackModeSpread {
		t.Errorf("compiled pack mode = %q, want %q", got, model.PackModeSpread)
	}
}
