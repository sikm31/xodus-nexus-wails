package shared

import (
	"encoding/json"
	"testing"
)

func TestCPULimit(t *testing.T) {
	if CPUTopology(0) != "" || CPUTopology(3) != "3:0,1,2" {
		t.Errorf("topology = %q / %q", CPUTopology(0), CPUTopology(3))
	}
	base := `{"mangoHud":false,"gamescope":false,"gamescopeArgs":"","launchOptions":""`
	for raw, want := range map[string]int{base + `}`: 0, base + `,"cpuLimit":8}`: 8, base + `,"cpuLimit":null}`: 0} {
		if tools, err := NormalizeLaunchTools(json.RawMessage(raw)); err != nil || tools.CPULimit != want {
			t.Errorf("%s -> %+v, %v", raw, tools, err)
		}
	}
	for _, raw := range []string{base + `,"cpuLimit":-1}`, base + `,"cpuLimit":65}`, base + `,"cpuLimit":"8"}`} {
		if _, err := NormalizeLaunchTools(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
