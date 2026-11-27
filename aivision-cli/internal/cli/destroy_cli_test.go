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
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "aivision-clone.json"), []byte(`{"id":"id"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.NewState(dir, "repo-prompt").RecordManagedRepo(repo, "id"); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "src", "stale")
	if err := os.MkdirAll(filepath.Join(stale, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, ".git", "aivision-clone.json"), []byte(`{"id":"different"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.NewState(dir, "repo-prompt").RecordManagedRepo(stale, "id"); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIStderr(t, "n\n", func() error {
		return cmdDestroy([]string{
			"--file", "../../blueprints/aivision.yaml",
			"--workdir", dir,
			"--source-root", filepath.Join(dir, "src"),
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
	if strings.Contains(out, stale) {
		t.Fatalf("stale record was listed for deletion: %q", out)
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

func TestDestroyPurgeIgnoresUnavailableRecordStorage(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "teardown-ran")
	state := runtime.NewState(dir, "aivision")
	if err := state.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, ".aivision-repos")
	if err := os.WriteFile(blocker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	bpPath := filepath.Join(dir, "blueprint.yaml")
	content := fmt.Sprintf(`metadata: {name: record-storage, namespace: aivision}
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

	if err := cmdDestroy([]string{"--file", bpPath, "--workdir", dir, "--namespace", "aivision", "--purge", "-y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("teardown did not run: %v", err)
	}
	if _, err := os.Stat(state.Root); !os.IsNotExist(err) {
		t.Fatalf("state directory survived the purge: %v", err)
	}
	if data, err := os.ReadFile(blocker); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated record path changed: %q, %v", data, err)
	}
}

func TestDestroyStatePreflightRefusesBeforeVolumeRemoval(t *testing.T) {
	workdir := t.TempDir()
	sourceRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workdir, "aivision")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	capture := filepath.Join(t.TempDir(), "docker-args")
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AIV_DOCKER_CAPTURE\"\ncase \"$1\" in\n  container) echo \"No such container: $3\" >&2 ;;\n  volume) echo \"No such volume: $3\" >&2 ;;\nesac\nexit 1\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_BIN", fakeDocker)
	t.Setenv("AIV_DOCKER_CAPTURE", capture)

	bpPath := filepath.Join(t.TempDir(), "blueprint.yaml")
	content := fmt.Sprintf(`metadata: {name: vol-test, namespace: aivision}
config:
  workdir: %s
  sourceRoot: %s
  dockerNetwork: test
components:
  - {name: demo, kind: container, image: busybox, volumes: ["aivision-data:/data"]}
`, workdir, sourceRoot)
	if err := os.WriteFile(bpPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdDestroy([]string{"--file", bpPath, "--workdir", workdir, "--namespace", "aivision", "--purge", "-y"}); err == nil {
		t.Fatal("destroy accepted a state root that resolves outside workdir")
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "volume rm") {
		t.Fatalf("volume removal ran despite the state preflight failure: %q", args)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("state target outside workdir was modified: %v", err)
	}
}
