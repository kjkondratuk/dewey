package safety

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The reported bug: a local model rates `echo hello > f.txt` as kind=write
// risk=safe, and gating on the tier alone let it run with no prompt.
func TestRequiresApproval_WriteAlwaysPromptsRegardlessOfTier(t *testing.T) {
	cases := []struct {
		name string
		c    Classification
		want bool
	}{
		{"write rated safe", Classification{Kind: "write", Risk: Safe}, true},
		{"write rated moderate", Classification{Kind: "write", Risk: Moderate}, true},
		{"write rated dangerous", Classification{Kind: "write", Risk: Dangerous}, true},
		{"read rated safe", Classification{Kind: "read", Risk: Safe}, false},
		{"read rated moderate", Classification{Kind: "read", Risk: Moderate}, true},
		{"override, no kind, safe", Classification{Risk: Safe}, false},
		{"override, no kind, moderate", Classification{Risk: Moderate}, true},
	}
	for _, c := range cases {
		if got := c.c.RequiresApproval(); got != c.want {
			t.Errorf("%s: RequiresApproval() = %v, want %v", c.name, got, c.want)
		}
	}
}

func floorOf(t *testing.T, command string) Classification {
	t.Helper()
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		t.Fatal(err)
	}
	return testRules(t).ShellFloor(string(raw))
}

func assertWrites(t *testing.T, commands ...string) {
	t.Helper()
	for _, c := range commands {
		if got := floorOf(t, c); got.Kind != "write" {
			t.Errorf("ShellFloor(%q) = %+v, want Kind=write", c, got)
		}
	}
}

func assertReads(t *testing.T, commands ...string) {
	t.Helper()
	for _, c := range commands {
		if got := floorOf(t, c); got.Kind == "write" {
			t.Errorf("ShellFloor(%q) = %+v, want no floor — this prompts on a read", c, got)
		}
	}
}

func TestShellFloor_AlwaysWritingPrograms(t *testing.T) {
	assertWrites(t,
		"rm -rf dist", "mv a b", "cp a b", "touch f", "mkdir -p x/y",
		"ln -s a b", "chmod +x s.sh", "chown me f", "dd if=a of=b",
		"truncate -s 0 f", "/bin/rm f", "sudo anything", "kill -9 123",
		"rsync -a a/ b/", "wget https://example.com/x", "scp a host:b",
		"crontab -e", "mount /dev/x /mnt",
	)
}

func TestShellFloor_PlainReadsAreNotFloored(t *testing.T) {
	assertReads(t,
		"ls", "ls -la", "cat go.mod", "grep -rn foo .", "head -20 f",
		"wc -l f", "ps aux", "df -h", "which go", "echo hello",
		"ls -la 2>&1", "pwd", "whoami", "date", "env",
	)
}

// The complaint that prompted this: flagging a whole program because it has
// write subcommands fires the gate on the most common commands there are.
func TestShellFloor_GitReadSubcommandsAreNotFloored(t *testing.T) {
	assertReads(t,
		"git status", "git log", "git log --oneline -20", "git diff",
		"git diff --staged", "git show HEAD", "git blame f.go",
		"git rev-parse HEAD", "git ls-files", "git describe --tags",
		"git shortlog -sn", "git grep foo", "git reflog", "git merge-base a b",
		"git cat-file -p HEAD", "git version", "git help",
		// Global flags must not be mistaken for the subcommand.
		"git -C /tmp/repo status", "git -c core.pager=cat log",
		"git --git-dir=/tmp/x/.git status",
		// A bare invocation prints usage.
		"git",
	)
}

func TestShellFloor_GitWriteSubcommandsAreFloored(t *testing.T) {
	assertWrites(t,
		"git add .", "git commit -m x", "git push", "git pull",
		"git reset --hard", "git checkout main", "git switch main",
		"git merge main", "git rebase main", "git clean -fd",
		"git rm f", "git mv a b", "git apply p.patch", "git cherry-pick abc",
		"git restore f", "git fetch", "git clone https://example.com/x",
		"git -C /tmp/repo commit -m x",
		// Unknown subcommands fail safe.
		"git some-new-command",
	)
}

// `git branch` lists; `git branch -d old` deletes. The operand is the tell.
func TestShellFloor_ListingSubcommandsDependOnOperands(t *testing.T) {
	assertReads(t,
		"git branch", "git branch -a", "git branch --list", "git branch -vv",
		"git tag", "git tag -l", "git remote", "git remote -v",
		"git stash list", "git config --get user.name", "git worktree list",
	)
	assertWrites(t,
		"git branch -d old", "git branch -D old", "git branch newthing",
		"git branch -m old new", "git tag v1.0.0", "git tag -d v1.0.0",
		"git remote add origin https://example.com/x", "git remote remove origin",
		"git stash", "git stash pop", "git config user.name Me",
		"git worktree add ../wt",
	)
}

func TestShellFloor_ContainerAndClusterReads(t *testing.T) {
	assertReads(t,
		"docker ps", "docker ps -a", "docker images", "docker logs c1",
		"docker inspect c1", "docker version", "docker stats",
		"kubectl get pods", "kubectl describe pod x", "kubectl logs x",
		"kubectl -n prod get svc", "kubectl version",
		"helm list", "helm status rel", "helm template .",
		"systemctl status nginx", "systemctl is-active nginx",
	)
	assertWrites(t,
		"docker run -it x", "docker rm c1", "docker rmi img", "docker build .",
		"docker exec c1 sh", "docker compose up",
		"kubectl apply -f x.yaml", "kubectl delete pod x",
		"kubectl -n prod delete svc y", "kubectl scale deploy x --replicas=3",
		"helm install rel chart", "helm upgrade rel chart", "helm uninstall rel",
		"systemctl restart nginx", "systemctl enable nginx",
	)
}

func TestShellFloor_PackageAndBuildTooling(t *testing.T) {
	assertReads(t,
		"npm ls", "npm view react", "npm outdated", "npm why react",
		"yarn list", "pnpm why react",
		"go version", "go env", "go list ./...", "go doc fmt", "go vet ./...",
		"cargo check", "cargo tree",
		"pip list", "pip show requests",
		"brew list", "brew info go", "brew search go",
		"mise ls", "mise current",
	)
	assertWrites(t,
		"npm install", "npm i react", "npm ci", "npm publish", "npm uninstall x",
		"yarn add react", "pnpm install",
		"go build ./...", "go get example.com/x", "go install x", "go mod tidy",
		"go run .", "go test ./...",
		"cargo build", "cargo install ripgrep",
		"pip install requests", "pip uninstall requests",
		"brew install go", "brew upgrade", "mise install",
	)
}

// Some programs read by default and write only with a particular flag.
func TestShellFloor_FlagDependentPrograms(t *testing.T) {
	assertReads(t,
		"sed s/a/b/ f", "sed -n 1,5p f",
		"curl https://example.com", "curl -s https://example.com",
		"find . -name '*.go'", "find . -type f",
		"tar -tf a.tar", "tar --list -f a.tar",
	)
	assertWrites(t,
		"sed -i s/a/b/ f", "sed --in-place s/a/b/ f",
		"curl -o out.bin https://example.com", "curl -O https://example.com",
		"curl --output out https://example.com", "curl -T up.txt https://example.com",
		"find . -name '*.tmp' -delete", "find . -exec rm {} ;",
		"tar -xzf a.tar.gz", "tar -czf a.tar.gz dir", "unzip a.zip",
	)
}

func TestShellFloor_RedirectionAndPipelines(t *testing.T) {
	assertWrites(t,
		"echo hello > f.txt", "echo hello >> f.txt",
		"cat a.txt | tee b.txt",
		"ls && rm f.txt", "grep -rn x . ; touch done",
		"git status && git commit -m x",
		"FOO=1 rm f.txt", "env rm f.txt",
	)
	assertReads(t,
		"ls -la 2>&1", "grep -rn foo . | head -20",
		"git log --oneline | head", "cat f | wc -l",
		"git status && git diff",
	)
}

func TestShellFloor_IgnoresUnparseableArguments(t *testing.T) {
	rules := testRules(t)
	for _, args := range []string{``, `not json`, `{}`, `{"command":""}`, `{"other":"x"}`} {
		if got := rules.ShellFloor(args); got.Kind != "" {
			t.Errorf("ShellFloor(%q) = %+v, want the zero value", args, got)
		}
	}
}

// The floor must only ever raise. A model that escalates past it keeps its
// higher tier; a model that rates a write "safe" gets corrected.
func TestRaiseToFloor_OnlyRaises(t *testing.T) {
	write := `{"command":"rm -rf dist"}`

	got := testRules(t).raiseToFloor(Classification{Kind: "read", Risk: Safe}, "Bash", write)
	if got.Kind != "write" || got.Risk != Moderate {
		t.Errorf("floor did not raise a read/safe write: %+v", got)
	}

	got = testRules(t).raiseToFloor(Classification{Kind: "write", Risk: Dangerous}, "Bash", write)
	if got.Risk != Dangerous {
		t.Errorf("floor lowered the model's tier: %+v, want Dangerous preserved", got)
	}

	// A read the floor doesn't recognize is left exactly as the model rated it.
	got = testRules(t).raiseToFloor(Classification{Kind: "read", Risk: Safe}, "Bash", `{"command":"git status"}`)
	if got.Kind != "read" || got.Risk != Safe {
		t.Errorf("floor raised a read subcommand: %+v", got)
	}

	// remember writes to the user's profile by definition, so it is floored
	// regardless of what the classifier decides.
	got = testRules(t).raiseToFloor(Classification{Kind: "read", Risk: Safe}, "remember", `{"text":"x"}`)
	if got.Kind != "write" {
		t.Errorf("remember was not floored as a write: %+v", got)
	}

	// A tool in neither category is left to the classifier entirely.
	got = testRules(t).raiseToFloor(Classification{Kind: "read", Risk: Safe}, "some_other_tool", `{"x":"1"}`)
	if got.Kind != "read" || got.Risk != Safe {
		t.Errorf("floor applied to an unlisted tool: %+v", got)
	}
}

// Every program with a read-only subcommand set must also appear in the
// global-flag table or not need one; a missing entry silently misreads the
// subcommand when a global flag takes a value.
func TestShellFloor_GlobalFlagHandlingCoversSubcommandPrograms(t *testing.T) {
	samples := map[string]string{
		"git":       "git -C /tmp status",
		"kubectl":   "kubectl -n prod get pods",
		"helm":      "helm -n prod list",
		"docker":    "docker -H unix:///x ps",
		"systemctl": "systemctl -H host status nginx",
	}
	for program, command := range samples {
		if got := floorOf(t, command); got.Kind == "write" {
			t.Errorf("%s: %q floored as a write; a global flag's value was read as the subcommand", program, command)
		}
	}
}

func TestShellFloor_UnknownProgramsAreLeftToTheClassifier(t *testing.T) {
	assertReads(t, "mycustomtool --do-something", "./scripts/deploy.sh", "jq .x f.json")
}

func TestAlwaysRead_FastPathOnlyInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "pkg", "file.go")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var r ShellRules

	if !r.AlwaysRead("Read", `{"file_path":`+quote(inside)+`}`, root) {
		t.Error("AlwaysRead(Read, inside root) = false, want true")
	}
	if r.AlwaysRead("Read", `{"file_path":`+quote(outside)+`}`, root) {
		t.Error("AlwaysRead(Read, outside root) = true, want false — it must be classified")
	}
}

func TestAlwaysRead_SymlinkCannotEscapeTheRoot(t *testing.T) {
	// A link inside the project pointing out of it must not inherit the fast
	// path; otherwise the confinement is decorative.
	root := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var r ShellRules
	if r.AlwaysRead("Read", `{"file_path":`+quote(link)+`}`, root) {
		t.Error("AlwaysRead(symlink out of root) = true, want false")
	}
}

func TestAlwaysRead_RejectsNonReadOnlyAndUnknownTools(t *testing.T) {
	root := t.TempDir()
	args := `{"file_path":` + quote(filepath.Join(root, "f")) + `}`
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var r ShellRules
	for _, name := range []string{shellToolName, "Write", "Edit", "remember", "Task", "mcp__x__y", ""} {
		if r.AlwaysRead(name, args, root) {
			t.Errorf("AlwaysRead(%q) = true, want false — only dewey's read-only tools qualify", name)
		}
	}
}

func TestAlwaysRead_WriteListStillWins(t *testing.T) {
	// Even for a read-only tool inside the root, an always-write pin outranks
	// the fast path.
	root := t.TempDir()
	path := filepath.Join(root, "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := ShellRules{AlwaysWriteTools: []string{"Read"}}

	if r.AlwaysRead("Read", `{"file_path":`+quote(path)+`}`, root) {
		t.Error("AlwaysRead = true, want false when the tool is pinned to write")
	}
}

func TestAlwaysRead_FailsClosedOnAnUnusablePathOrRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "exists")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := `{"file_path":` + quote(path) + `}`

	var r ShellRules
	cases := []struct {
		name, argsJSON, root string
	}{
		{"empty root", args, ""},
		{"nonexistent path", `{"file_path":` + quote(filepath.Join(root, "absent")) + `}`, root},
		{"malformed args", `{not json`, root},
		{"wrong arg key", `{"path":` + quote(path) + `}`, root},
	}
	for _, c := range cases {
		if r.AlwaysRead("Read", c.argsJSON, c.root) {
			t.Errorf("AlwaysRead(%s) = true, want false — must fail closed", c.name)
		}
	}
}

func TestAlwaysRead_GrepAndGlobUseThePathArgument(t *testing.T) {
	root := t.TempDir()
	var r ShellRules

	for _, tool := range []string{"Grep", "Glob"} {
		if !r.AlwaysRead(tool, `{"pattern":"x","path":`+quote(root)+`}`, root) {
			t.Errorf("AlwaysRead(%s, path inside root) = false, want true", tool)
		}
		if r.AlwaysRead(tool, `{"pattern":"x","path":`+quote(t.TempDir())+`}`, root) {
			t.Errorf("AlwaysRead(%s, path outside root) = true, want false", tool)
		}
	}
}

// quote renders s as a JSON string literal, so test paths containing
// separators or spaces survive embedding in an argument blob.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestAlwaysRead_RefusesAHomeDirectoryRoot(t *testing.T) {
	// ProjectRoot stops its upward walk at a .dewey directory, and ~/.dewey
	// exists for anyone who has ever saved a memory — so every non-git
	// directory under home resolves the "project" to the whole home
	// directory. Treating that as confinement would put ~/.ssh, ~/.aws and
	// ~/.gnupg back on the no-prompt path, which is the hole confinement was
	// added to close.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	var r ShellRules
	// The home directory itself is guaranteed to exist, so EvalSymlinks
	// resolves and the containment check genuinely reaches the guard.
	args := `{"file_path":` + quote(home) + `}`

	if r.AlwaysRead("Read", args, home) {
		t.Error("AlwaysRead(root=$HOME) = true, want false — home is not a project")
	}
	if r.AlwaysRead("Read", args, filepath.Dir(home)) {
		t.Error("AlwaysRead(root=parent of $HOME) = true, want false — an ancestor of home is not a project either")
	}
}
