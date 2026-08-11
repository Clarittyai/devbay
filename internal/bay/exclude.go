package bay

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// excludeMarker introduces the block devbay owns, so the file can be rewritten
// without disturbing anything a developer put there.
const excludeMarker = "# devbay: generated test reports"

// excludeGenerated tells git to ignore the artifacts devbay's tasks produce.
//
// Every task with a `report` writes a file into the worktree, because that is
// the only place both the container and the host can see. Left alone, running
// the test suite once makes the checkout dirty, which has two costs: `git
// status` fills with XML nobody wrote, and `devbay rm` refuses to remove the
// bay because it looks like there is unsaved work to lose.
//
// This is written to .git/info/exclude rather than .gitignore because it is a
// property of this checkout, not of the project. Committing devbay's paths into
// a repository that may not use devbay would be presumptuous, and a .gitignore
// change would itself be an uncommitted modification.
func excludeGenerated(worktree string, m *manifest.Manifest) error {
	paths := reportPaths(m)
	if len(paths) == 0 {
		return nil
	}
	prepareReportDirs(worktree, m)

	dir, err := gitInfoDir(worktree)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file := filepath.Join(dir, "exclude")

	var kept []string
	if f, err := os.Open(file); err == nil {
		sc := bufio.NewScanner(f)
		inBlock := false
		for sc.Scan() {
			line := sc.Text()
			if line == excludeMarker {
				inBlock = true
				continue
			}
			// The block runs to the next blank line, so rewriting it is
			// idempotent and never accumulates duplicates across boots.
			if inBlock {
				if strings.TrimSpace(line) == "" {
					inBlock = false
				}
				continue
			}
			kept = append(kept, line)
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return err
	}

	var b strings.Builder
	for _, l := range kept {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) != "" {
		b.WriteByte('\n')
	}
	b.WriteString(excludeMarker)
	b.WriteByte('\n')
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	return os.WriteFile(file, []byte(b.String()), 0o644)
}

// reportPaths collects the paths tasks write, plus the directories holding
// them: a runner usually creates the directory as well as the file, and an
// untracked directory is just as dirty as an untracked file.
func reportPaths(m *manifest.Manifest) []string {
	set := map[string]bool{}
	for _, t := range m.Tasks {
		if t == nil || t.Report == nil || t.Report.Path == "" {
			continue
		}
		p := strings.TrimPrefix(filepath.ToSlash(t.Report.Path), "./")
		if p == "" || strings.HasPrefix(p, "..") {
			continue
		}
		set["/"+p] = true
		if dir := filepath.ToSlash(filepath.Dir(p)); dir != "." && dir != "/" {
			set["/"+dir+"/"] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// gitInfoDir returns the directory holding the exclude file git actually reads.
//
// It must be the COMMON git directory, not the per-worktree one. A linked
// worktree has its own git directory under .git/worktrees/<name>, and writing
// info/exclude there has no effect at all -- verified directly: with the file
// in the per-worktree directory `git status` still reports the path as
// untracked, and only the common directory suppresses it.
//
// The consequence is that the exclusion is shared by every worktree of the
// repository, which is correct anyway: the report paths come from the manifest
// and are the same in every bay.
func gitInfoDir(worktree string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locating the git directory for %s: %w", worktree, err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "info"), nil
}

// prepareReportDirs creates the directories a task's report will be written
// into, while the worktree is still owned by this process.
//
// It has to happen now rather than at task time. Containers write into the
// bind-mounted worktree as root, and on Linux a container's uid is the
// filesystem's uid -- so once a bay has run, the developer's own process
// cannot create a directory inside its worktree. Creating it at task time
// failed with "permission denied" on every bay that had booted, and before
// that failure was surfaced it appeared as the test framework's own ENOENT on
// the report path, which names a file rather than a directory.
//
// World-writable because the container writes the report and does not run as
// this user.
func prepareReportDirs(worktree string, m *manifest.Manifest) {
	for _, t := range m.Tasks {
		if t == nil || t.Report == nil || t.Report.Path == "" {
			continue
		}
		rel := filepath.Dir(filepath.FromSlash(strings.TrimPrefix(t.Report.Path, "./")))
		if rel == "." || rel == string(filepath.Separator) || strings.HasPrefix(rel, "..") {
			continue
		}
		dir := filepath.Join(worktree, rel)
		if err := os.MkdirAll(dir, 0o777); err == nil {
			_ = os.Chmod(dir, 0o777)
		}
	}
}
