package ops_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/ops"
	"github.com/JamesPeck/pic-sure-cli/internal/stack"
)

func TestUpStepIDsMatchUpSteps(t *testing.T) {
	st, err := stack.Create(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, mode := range []stack.DBMode{stack.DBLocal, stack.DBRemote} {
		cfg := stack.DefaultConfig()
		cfg.Name, cfg.DB.Mode = "demo", mode
		var ids []string
		for _, s := range ops.UpSteps(&ops.Deps{}, st, &cfg, &stack.Secrets{}, &stack.State{}, ops.ConvergeOptions{}) {
			ids = append(ids, s.ID)
		}
		if want := ops.UpStepIDs(&cfg); !slices.Equal(ids, want) {
			t.Errorf("db.mode %s: UpSteps %v, UpStepIDs %v", mode, ids, want)
		}
		// up is init's plan without resolve, with the restart before start.
		init := ops.InitStepIDs(&cfg)
		if want := append(slices.Clone(init[1:len(init)-1]), ops.RestartStepID, ops.StartStepID); !slices.Equal(ids, want) {
			t.Errorf("db.mode %s: UpSteps %v, want %v", mode, ids, want)
		}
	}
}
