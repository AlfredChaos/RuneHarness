package permission

import (
	"encoding/json"
	"testing"
)

func args(s string) json.RawMessage { return json.RawMessage(s) }

func TestCheck(t *testing.T) {
	d := New(Config{
		SafeTools:           []string{"read_file", "list_dir"},
		SafeCommandPrefixes: []string{"ls", "git status"},
		ForbiddenPatterns:   []string{"rm -rf", "sudo "},
		ForbiddenPaths:      []string{".env"},
	})
	cases := []struct {
		name, tool, arg string
		want            Verdict
	}{
		{"safe tool", "read_file", `{"path":"main.go"}`, Allow},
		{"safe cmd", "run_command", `{"command":"ls -la"}`, Allow},
		{"safe prefix not fooled", "run_command", `{"command":"lsof -i"}`, Ask},
		{"metachar escalates", "run_command", `{"command":"ls && echo hi"}`, Ask},
		{"unknown cmd asks", "run_command", `{"command":"uptime"}`, Ask},
		{"forbidden cmd", "run_command", `{"command":"sudo ls"}`, Deny},
		{"forbidden path via read", "read_file", `{"path":".env"}`, Deny},
		{"forbidden path via shell", "run_command", `{"command":"cat .env"}`, Deny},
		{"env.example ok", "read_file", `{"path":".env.example"}`, Allow},
		// 读放开后的技能链路：~/.agents/skills 在 workspace 外，Gate1 不得误拦
		{"global skill path ok", "read_file", `{"path":"/home/u/.agents/skills/review/SKILL.md"}`, Allow},
		{"unknown tool asks", "write_file", `{"path":"a.txt"}`, Ask},
	}
	for _, c := range cases {
		if got, _ := d.Check(c.tool, args(c.arg)); got != c.want {
			t.Errorf("%s: Check(%s, %s) = %v, want %v", c.name, c.tool, c.arg, got, c.want)
		}
	}
}
