package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

func TestStoreWaited(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		waited time.Duration
		err    error
		want   bool
	}{
		{name: "fast call logs nothing", waited: 49 * time.Millisecond},
		{name: "at the threshold", waited: telemetry.SlowStoreCall, want: true},
		{name: "slow failure carries its error", waited: 250 * time.Millisecond, err: errors.New("i/o timeout"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			ctx := telemetry.WithRequest(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)), "main-7")
			telemetry.StoreWaited(ctx, telemetry.StoreCacheLookup, "get", tt.waited, tt.err)
			if !tt.want {
				if buf.Len() != 0 {
					t.Fatalf("logged %q", buf.String())
				}
				return
			}
			var line struct {
				Level, Msg, Component, Op, Error string
				RequestID                        string  `json:"request_id"`
				WaitMS                           float64 `json:"wait_ms"`
			}
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("%v: %q", err, buf.String())
			}
			wantErr := ""
			if tt.err != nil {
				wantErr = tt.err.Error()
			}
			if line.Level != "WARN" || line.Msg != telemetry.SlowStoreMessage || line.Component != "cache_lookup" ||
				line.Op != "get" || line.RequestID != "main-7" || line.Error != wantErr ||
				line.WaitMS != telemetry.Millis(tt.waited) {
				t.Fatalf("line = %+v", line)
			}
		})
	}
}
