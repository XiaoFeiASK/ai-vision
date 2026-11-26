package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antgroup/ai-vision/aivision-cli/internal/deploy/runtime"
)

func captureCLIStderr(t *testing.T, stdin string, fn func() error) (string, error) {
	t.Helper()
	oldIn, oldErr := os.Stdin, os.Stderr
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stderr = inR, outW
	defer func() {
		os.Stdin, os.Stderr = oldIn, oldErr
		_ = inR.Close()
		_ = outR.Close()
	}()
	if _, err := io.WriteString(inW, stdin); err != nil {
		t.Fatal(err)
	}
	_ = inW.Close()
	runErr := fn()
	_ = outW.Close()
	out, err := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

func TestDestroyRefusesUnsafeNamespaceBeforeTeardown(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "teardown-ran")
	bpPath := filepath.Join(dir, "blueprint.yaml")
	content := fmt.Sprintf(`metadata: {name: ns-test, namespace: aivision}
config:
  workdir: %s
  sourceRoot: %s
components:
  - name: demo
    kind: process
    run: "true"
    teardown:
      - shell: "touch %s"
`, dir, dir, marker)
	if err := os.WriteFile(bpPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdDestroy([]string{"--file", bpPath, "--workdir", dir, "--namespace", "../victim", "--purge", "-y"}); err == nil {
		t.Fatal("destroy accepted a namespace outside workdir")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("teardown ran before the namespace was rejected: %v", err)
	}
}

func TestDestroyConfirmationMatchesTarget(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		wantState bool
	}{
		{name: "docker-stops-state", target: "docker", wantState: true},
		{name: "compose-retains-state", target: "compose", wantState: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out, err := captureCLIStderr(t, "n\n", func() error {
				args := []string{
					"--file", "../../blueprints/aivision.yaml",
					"--workdir", dir,
					"--namespace", "compose-test",
					"--purge",
				}
				if tc.target != "docker" {
					args = append(args, "--target", tc.target)
				}
				return cmdDestroy(args)
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "Continue? [y/N]") {
				t.Fatalf("confirmation prompt missing: %q", out)
			}
			hasState := strings.Contains(out, "State directory to delete")
			if hasState != tc.wantState {
				t.Fatalf("state-directory line present=%v, want %v: %q", hasState, tc.wantState, out)
			}
			if tc.target == "compose" && !strings.Contains(out, "named volumes") {
				t.Fatalf("Compose volume warning missing: %q", out)
			}
		})
	}
}

func TestDestroyPromptListsRecordedRepos(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src", "backend")
	if err := runtime.NewState(dir, "repo-prompt").RecordManagedRepo(repo, "id"); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIStderr(t, "n\n", func() error {
		return cmdDestroy([]string{
			"--file", "../../blueprints/aivision.yaml",
			"--workdir", dir,
			"--namespace", "repo-prompt",
			"--purge-repos",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, repo) {
		t.Fatalf("recorded repo missing from confirmation: %q", out)
	}
}

func TestDestroyPurgeReposThroughCLI(t *testing.T) {
	dir := t.TempDir()
	sourceRoot := filepath.Join(dir, "src")
	cloned := filepath.Join(sourceRoot, "cloned")
	existing := filepath.Join(sourceRoot, "existing")
	for _, checkout := range []struct{ dir, id string }{
		{dir: cloned, id: "owner-id"},
		{dir: existing, id: ""},
	} {
		if err := os.MkdirAll(filepath.Join(checkout.dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout.dir, "file"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if checkout.id != "" {
			marker := []byte(`{"id":"` + checkout.id + `"}`)
			if err := os.WriteFile(filepath.Join(checkout.dir, ".git", "aivision-clone.json"), marker, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runtime.NewState(dir, "aivision").RecordManagedRepo(checkout.dir, checkout.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	bpPath := filepath.Join(dir, "blueprint.yaml")
	content := fmt.Sprintf(`metadata: {name: cli-destroy, namespace: aivision}
config:
  workdir: %s
  sourceRoot: %s
components:
  - {name: cloned, kind: process, run: "true", repo: {path: cloned}}
  - {name: existing, kind: process, run: "true", repo: {path: existing}}
`, dir, sourceRoot)
	if err := os.WriteFile(bpPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdDestroy([]string{"--file", bpPath, "--workdir", dir, "--namespace", "aivision", "--purge-repos", "-y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cloned); !os.IsNotExist(err) {
		t.Fatalf("aiv clone was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existing, "file")); err != nil {
		t.Fatalf("pre-existing checkout was removed: %v", err)
	}
}
