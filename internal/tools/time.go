package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// CurrentTime 返回当前时间，可选 IANA 时区。
type CurrentTime struct{}

func (CurrentTime) Spec() Spec {
	return Spec{
		Name:        "get_current_time",
		Description: "Get the current date and time",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"timezone": map[string]any{"type": "string", "description": "IANA timezone, e.g. Asia/Shanghai; defaults to local"},
			},
		},
	}
}

func (CurrentTime) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Timezone string `json:"timezone"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	loc := time.Local
	if args.Timezone != "" {
		l, err := time.LoadLocation(args.Timezone)
		if err != nil {
			return "", fmt.Errorf("invalid timezone %q", args.Timezone)
		}
		loc = l
	}
	return time.Now().In(loc).Format(time.RFC3339), nil
}
