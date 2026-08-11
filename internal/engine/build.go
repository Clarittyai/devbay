package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// buildImage builds a service's image from the repository.
//
// Most real repositories describe their own application with a Dockerfile
// rather than a published image -- three of the five services in a stock
// compose file usually do -- so a devbay that could only pull images could not
// run the application it was pointed at, only its database and its cache.
//
// The context comes from inside the bay's worktree, which is what makes this
// interesting for isolation: two bays on two branches build two different
// images from two different checkouts, and neither can see the other's.
func (e *Engine) buildImage(ctx context.Context, name string, s *manifest.Service) (string, error) {
	root, err := e.buildContext(s)
	if err != nil {
		return "", err
	}
	dockerfile := s.Build.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	// Checked before the tar is built so a typo is reported as a missing
	// Dockerfile rather than as a build failure a hundred lines later.
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dockerfile))); err != nil {
		return "", fmt.Errorf("build: no %s in %s", dockerfile, root)
	}

	// The tag is derived from what goes into the image, so an unchanged tree
	// reuses the previous build and a changed one does not. Docker's layer
	// cache does the real work; this keeps the *tag* honest, so two bays on
	// different branches never share one.
	tag, err := e.buildTag(name, root, dockerfile, s.Build.Target)
	if err != nil {
		return "", err
	}

	if have, err := e.haveImage(ctx, tag); err == nil && have {
		return tag, nil
	}

	e.Log("  building %s from %s", name, rel(e.worktree, root))
	tarball, err := tarDirectory(root)
	if err != nil {
		return "", fmt.Errorf("build: packing %s: %w", root, err)
	}

	resp, err := e.cli.ImageBuild(ctx, tarball, client.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: dockerfile,
		Target:     s.Build.Target,
		Remove:     true,
		// Labelled so teardown can find it. An image built for a bay that no
		// longer exists is exactly the kind of thing that accumulates
		// invisibly until a machine runs out of disk -- which is not a
		// hypothetical, it is how the first build test on this machine failed.
		Labels: e.labels(name),
	})
	if err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	defer resp.Body.Close()

	if err := drainBuild(resp.Body, func(line string) {
		e.Log("    %s", line)
	}); err != nil {
		return "", fmt.Errorf("build %s: %w", name, err)
	}
	return tag, nil
}

// buildContext resolves the build context and confines it to the worktree.
//
// A context path is manifest content, and a manifest may have been generated
// from repository content, so `context: ../../..` has to be refused rather
// than trusted: it would hand the daemon whatever is above the checkout --
// including, on a normal machine, the developer's home directory.
func (e *Engine) buildContext(s *manifest.Service) (string, error) {
	dir := s.Build.Context
	if dir == "" {
		dir = "."
	}
	if filepath.IsAbs(filepath.FromSlash(dir)) {
		return "", fmt.Errorf("build: context %q must be relative to the repository", dir)
	}

	root, err := filepath.Abs(filepath.Join(e.worktree, filepath.FromSlash(dir)))
	if err != nil {
		return "", err
	}
	// Symlinks are resolved before the check, or a link inside the worktree
	// pointing out of it would walk straight past it.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	base := e.worktree
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	if root != base && !strings.HasPrefix(root, base+string(filepath.Separator)) {
		return "", fmt.Errorf("build: context %q escapes the worktree", dir)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("build: context %s does not exist", rel(e.worktree, root))
	}
	return root, nil
}

// buildTag names the image after the content it is built from.
func (e *Engine) buildTag(service, root, dockerfile, target string) (string, error) {
	sum, err := hashTree(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(sum + "\x00" + dockerfile + "\x00" + target))
	digest := hex.EncodeToString(h.Sum(nil))[:12]
	return fmt.Sprintf("devbay-%s-%s:%s", slugForTag(e.m.Project), slugForTag(service), digest), nil
}

func (e *Engine) haveImage(ctx context.Context, tag string) (bool, error) {
	found, err := e.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", tag),
	})
	if err != nil {
		return false, err
	}
	return len(found.Items) > 0, nil
}

// removeBuiltImages deletes images this bay built.
//
// Teardown being total is the promise, and an image is the largest thing a bay
// creates: leaving them behind would fill a disk far faster than any container
// or volume.
func (e *Engine) removeBuiltImages(ctx context.Context) error {
	list, err := e.cli.ImageList(ctx, client.ImageListOptions{
		All:     true,
		Filters: e.filter(),
	})
	if err != nil {
		return err
	}
	var errs []string
	for _, img := range list.Items {
		if _, err := e.cli.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{
			// Not forced: another bay on the same commit legitimately shares
			// this image, and Docker refuses while a container still uses it.
			// That refusal is correct, so it is ignored rather than overridden.
			PruneChildren: true,
		}); err != nil && !isNotFound(err) && !strings.Contains(err.Error(), "is being used") {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("removing built images: %s", strings.Join(errs, "; "))
	}
	return nil
}

// hashTree fingerprints a directory: names, modes and contents.
//
// Walked in sorted order so the same tree always produces the same digest,
// which is the only reason the tag can be used as a cache key at all.
func hashTree(root string) (string, error) {
	h := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing in these belongs in an image, and .git in particular
			// makes the digest change on every commit even when the tree does
			// not.
			switch d.Name() {
			case ".git", "node_modules", ".venv", "__pycache__", "target", "dist":
				if path != root {
					return fs.SkipDir
				}
			}
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", filepath.ToSlash(rel), fi.Mode().Perm(), fi.Size())
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tarDirectory packs a build context for the daemon.
func tarDirectory(root string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".venv", "__pycache__":
				if path != root {
					return fs.SkipDir
				}
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		// Symlinks are recorded as links rather than followed. Following one
		// that points outside the context would copy a file the context does
		// not contain into the image.
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     filepath.ToSlash(name),
				Linkname: target,
				Mode:     int64(fi.Mode().Perm()),
			})
		}
		if !fi.Mode().IsRegular() {
			return nil // sockets, devices and fifos have no place in an image
		}

		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     filepath.ToSlash(name),
			Mode:     int64(fi.Mode().Perm()),
			Size:     fi.Size(),
		}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// drainBuild reads the daemon's build stream, surfacing progress and errors.
//
// The body must be consumed for the build to run to completion, and the
// failure is reported *inside* the stream rather than as an HTTP status -- so
// a caller that only checks the error from ImageBuild sees a successful build
// that produced no image.
func drainBuild(r io.Reader, log func(string)) error {
	dec := json.NewDecoder(r)
	var lastStep string
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
			Detail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != "" || msg.Detail != nil {
			detail := msg.Error
			if msg.Detail != nil && msg.Detail.Message != "" {
				detail = msg.Detail.Message
			}
			if lastStep != "" {
				return fmt.Errorf("%s (at %s)", strings.TrimSpace(detail), lastStep)
			}
			return fmt.Errorf("%s", strings.TrimSpace(detail))
		}
		line := strings.TrimSpace(msg.Stream)
		if line == "" {
			continue
		}
		// Steps are worth showing; the rest is layer noise that would bury the
		// one line a developer needs when a build fails.
		if strings.HasPrefix(line, "Step ") {
			lastStep = line
			// devbay adds its own labels so teardown can find the image, and
			// they arrive as build steps. Showing them makes a two-step
			// Dockerfile report six, and the developer looks for four steps
			// they did not write.
			if strings.Contains(line, "LABEL dev.devbay.") {
				continue
			}
			log(line)
		}
	}
}

// slugForTag makes a string safe for a Docker tag component.
func slugForTag(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "image"
	}
	return out
}

// rel renders a path relative to the worktree for a message a human reads.
//
// Both sides are resolved first. On macOS the worktree arrives as /var/... and
// the context resolves to /private/var/..., so a naive Rel between them walks
// up and out, and the message shows a full temporary path instead of ./web.
func rel(base, path string) string {
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	if r, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(r, "..") {
		if r == "." {
			return "."
		}
		return "./" + filepath.ToSlash(r)
	}
	return path
}
