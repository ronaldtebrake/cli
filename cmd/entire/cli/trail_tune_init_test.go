package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/runnerdefaults"
)

func TestRunnerDefaults_AreValidAndComplete(t *testing.T) {
	t.Parallel()

	files, err := runnerdefaults.Files()
	if err != nil {
		t.Fatalf("runnerdefaults.Files: %v", err)
	}
	if len(files) < 7 {
		t.Fatalf("expected at least 7 default runners, got %d", len(files))
	}
	for _, f := range files {
		var doc struct {
			ID     string `json:"id"`
			Output struct {
				ResultType string `json:"result_type"`
			} `json:"output"`
			Prompt struct {
				Template string `json:"template"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal(f.Data, &doc); err != nil {
			t.Errorf("%s: invalid JSON: %v", f.Name, err)
			continue
		}
		if doc.ID == "" || doc.Output.ResultType == "" || doc.Prompt.Template == "" {
			t.Errorf("%s: missing contract fields (id=%q result_type=%q template_empty=%v)",
				f.Name, doc.ID, doc.Output.ResultType, doc.Prompt.Template == "")
		}
	}
}

func TestEnsureRunnersPresent_CreatesDefaultsWhenEmpty(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	var out, errOut bytes.Buffer

	created, err := ensureRunnersPresent(&out, &errOut, repoRoot, true /* assumeYes */)
	if err != nil {
		t.Fatalf("ensureRunnersPresent: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for an empty repo")
	}

	written, err := filepath.Glob(filepath.Join(repoRoot, ".entire", "runners", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) < 7 {
		t.Fatalf("expected the default set written, got %d files", len(written))
	}
	// And every written file is loadable by the tuner.
	runners, err := loadTuneRunners(repoRoot, "")
	if err != nil {
		t.Fatalf("loadTuneRunners after scaffold: %v", err)
	}
	if len(runners) != len(written) {
		t.Errorf("loadTuneRunners saw %d, wrote %d", len(runners), len(written))
	}
}

func TestEnsureRunnersPresent_NoopWhenRunnersExist(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	dir := filepath.Join(repoRoot, ".entire", "runners")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trail-risk.json"),
		[]byte(`{"id":"trail-risk","prompt":{"template":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := ensureRunnersPresent(&bytes.Buffer{}, &bytes.Buffer{}, repoRoot, true)
	if err != nil {
		t.Fatalf("ensureRunnersPresent: %v", err)
	}
	if created {
		t.Error("expected created=false when runners already exist")
	}
}
