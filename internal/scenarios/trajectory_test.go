package scenarios

import "testing"

func call(plugin, action string, args map[string]string) ToolCallResult {
	return ToolCallResult{Plugin: plugin, Action: action, Args: args}
}

func TestCheckTrajectory(t *testing.T) {
	// A realistic AssetOpsBench-shaped plan: find sensors, then read each.
	traj := &Trajectory{
		Steps: []TrajStep{
			{ID: "sensors", Tool: "iot__asset_ids", Args: map[string]string{"site": "MAIN"}},
			{ID: "read", Tool: "iot__latest_reading", Args: map[string]string{"sensor": "*"}},
		},
		Links: []TrajLink{{From: "sensors", To: "read"}},
	}

	tests := []struct {
		name    string
		traj    *Trajectory
		result  RunResult
		wantErr bool
	}{
		{
			name: "dag satisfied in order",
			traj: traj,
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
				call("iot", "latest_reading", map[string]string{"sensor": "s1"}),
			}},
		},
		{
			name: "dag satisfied with an incidental extra call between",
			traj: traj,
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
				call("iot", "sensor_stats", map[string]string{"sensor": "s1"}),
				call("iot", "latest_reading", map[string]string{"sensor": "s1"}),
			}},
		},
		{
			name: "link violated: read before sensors",
			traj: traj,
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "latest_reading", map[string]string{"sensor": "s1"}),
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
			}},
			wantErr: true,
		},
		{
			name: "missing required step",
			traj: traj,
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
			}},
			wantErr: true,
		},
		{
			name: "wrong deterministic arg value",
			traj: traj,
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "BACKUP"}),
				call("iot", "latest_reading", map[string]string{"sensor": "s1"}),
			}},
			wantErr: true,
		},
		{
			name: "star arg requires the key to be present and non-empty",
			traj: traj,
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
				call("iot", "latest_reading", map[string]string{"sensor": ""}),
			}},
			wantErr: true,
		},
		{
			name: "allow_extra false rejects unbound call",
			traj: &Trajectory{
				Steps:      []TrajStep{{ID: "sensors", Tool: "iot__asset_ids", Args: map[string]string{"site": "MAIN"}}},
				AllowExtra: boolPtr(false),
			},
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
				call("iot", "latest_reading", map[string]string{"sensor": "s1"}),
			}},
			wantErr: true,
		},
		{
			name: "optional step absent is fine",
			traj: &Trajectory{
				Match: "set",
				Steps: []TrajStep{
					{ID: "sensors", Tool: "iot__asset_ids", Args: map[string]string{"site": "MAIN"}},
					{ID: "maybe", Tool: "iot__history", Optional: true},
				},
			},
			result: RunResult{ToolCalls: []ToolCallResult{
				call("iot", "asset_ids", map[string]string{"site": "MAIN"}),
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkTrajectory(tc.traj, tc.result)
			if tc.wantErr && got == "" {
				t.Fatalf("expected failure, got pass")
			}
			if !tc.wantErr && got != "" {
				t.Fatalf("expected pass, got %q", got)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
