package cli

import (
	"context"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/docker"
	"github.com/JamesPeck/pic-sure-cli/internal/events"
)

func TestUpPreviewRejectsUnsafeProbe(t *testing.T) {
	for _, opts := range []docker.RunOpts{
		{Remove: true, Network: "bridge"},
		{Network: "none"},
		{Remove: true, Network: "none", Mounts: []docker.Mount{{Source: "volume", Target: "/data"}}},
	} {
		engine := &upProbeEngine{report: &upPlan{}, sink: events.Discard}
		if _, err := engine.Run(context.Background(), opts); err == nil {
			t.Fatal("unsafe probe reached Docker")
		}
		if err := engine.Rm(context.Background(), opts.Name, true); err != nil {
			t.Fatalf("rejected probe cleanup: %v", err)
		}
	}
}
