package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Seeding makes the second bay of a project cheap.
//
// A migration suite is the slowest part of bringing a datastore up, it is
// identical in every bay, and it is the reason a developer stops opening a
// second bay: paying ninety seconds to look at a branch is a decision, and
// most of the time the decision is no.
//
// The mechanism is a per-project template volume rather than a template
// database. `CREATE DATABASE ... TEMPLATE` copies at O(size) with no other
// session connected, and five parallel bays either serialize behind that lock
// or fail -- measured at 67 seconds for a 6 GB database. A volume copied by
// tar has neither the lock nor the connection requirement, runs in parallel,
// and needs no knowledge of the database's own tooling.
//
// The template is captured from the first bay that seeds successfully, not
// built separately. A separate builder would be a second boot path, and the
// state it produced would be one nothing had ever run against.
const seedPrefix = "devbay-seed-"

// LabelSeed marks a template volume, which is project-scoped rather than
// bay-scoped: that is the whole point of it, and it is also why teardown has
// to treat it specially.
const LabelSeed = "dev.devbay.seed"

// stateDirs reports where an image keeps its state.
//
// Read from the image's own VOLUME declaration rather than a table of
// databases devbay knows. postgres declares /var/lib/postgresql/data, mysql
// /var/lib/mysql, mongo /data/db, redis /data -- every datastore image
// declares the directory it must not lose, because that declaration is what
// makes `docker run` without a volume merely wasteful instead of broken. A
// table would be devbay guessing at something the image already states.
func (e *Engine) stateDirs(ctx context.Context, image string) ([]string, error) {
	insp, err := e.cli.ImageInspect(ctx, image)
	if err != nil {
		return nil, err
	}
	if insp.Config == nil {
		return nil, nil
	}
	out := make([]string, 0, len(insp.Config.Volumes))
	for path := range insp.Config.Volumes {
		out = append(out, strings.TrimRight(path, "/"))
	}
	sort.Strings(out)
	return out, nil
}

// seedHash decides when a template is stale.
//
// Over the contents of the declared sources, not their modification times: a
// checkout, a rebase and a branch switch all rewrite mtimes without changing a
// migration, and rebuilding on each of those would make the cache miss exactly
// when a developer is moving between branches -- which is when they are
// creating bays.
//
// The image and the seeding commands are in the hash too. A template built
// against postgres:15 is not a postgres:16 template, and a changed seed
// command is a changed result.
func (e *Engine) seedHash(name string, s *manifest.Service) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%s\n", name, s.Image)

	for _, step := range s.Seed.After {
		dep, ok := e.m.Services[step]
		if !ok {
			continue
		}
		fmt.Fprintf(h, "step %s %s %v %v\n", step, dep.Image, dep.Install, dep.Run)
	}

	files, err := e.seedSources(s.Seed.Sources)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		// Refused rather than hashed as empty. An empty match means the globs
		// are wrong, and a template keyed on nothing would serve last month's
		// schema until someone worked out why.
		return "", fmt.Errorf("seed sources %v matched no files in the worktree", s.Seed.Sources)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(e.worktree, f)
		fmt.Fprintf(h, "%s %x\n", filepath.ToSlash(rel), sha256.Sum256(b))
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// seedSources expands the declared globs, sorted, confined to the worktree.
func (e *Engine) seedSources(globs []string) ([]string, error) {
	seen := map[string]bool{}
	for _, g := range globs {
		if filepath.IsAbs(g) || strings.Contains(g, "..") {
			return nil, fmt.Errorf("seed source %q leaves the worktree", g)
		}
		// A bare directory means everything under it, which is what
		// `sources: [db/migrations]` plainly reads as.
		full := filepath.Join(e.worktree, g)
		if fi, err := os.Stat(full); err == nil && fi.IsDir() {
			err := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					seen[p] = true
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		matches, err := filepath.Glob(full)
		if err != nil {
			return nil, fmt.Errorf("seed source %q: %w", g, err)
		}
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
				seen[m] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// seedState is what one service's seeding resolved to for this bay.
type seedState struct {
	// Mounts are the named volumes to attach at the image's state paths.
	Mounts []mount.Mount
	// Volumes maps each state path to this bay's volume.
	Volumes map[string]string
	// Templates maps each state path to the project template's volume.
	Templates map[string]string
	// Restored is true when a template already existed and was copied in, so
	// the seeding steps have nothing left to do.
	Restored bool
	// Existing is true when this bay's own volume was already there, which
	// means its state is the developer's and neither restoring nor capturing
	// may touch it.
	Existing bool
	Hash     string
}

// prepareSeed resolves a seeded service's volumes before its container exists.
func (e *Engine) prepareSeed(ctx context.Context, name string, s *manifest.Service) (*seedState, error) {
	if s.Seed == nil || s.Fork != manifest.ForkImage {
		return nil, nil
	}
	dirs, err := e.stateDirs(ctx, s.Image)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		// Nothing to capture, so seeding would silently do nothing -- which is
		// the failure this whole change exists to remove.
		return nil, fmt.Errorf("%s: %s declares no state directory, so there is nothing to seed; "+
			"remove seed: or use an image that declares a VOLUME", name, s.Image)
	}
	hash, err := e.seedHash(name, s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	st := &seedState{
		Volumes:   map[string]string{},
		Templates: map[string]string{},
		Hash:      hash,
	}
	restore := true
	for _, dir := range dirs {
		tmpl := seedPrefix + e.m.Project + "-" + name + "-" + slug(dir) + "-" + hash
		vol := e.containerName(name) + "-" + slug(dir)
		st.Templates[dir] = tmpl
		st.Volumes[dir] = vol
		if !e.volumeExists(ctx, tmpl) {
			restore = false
		}
		// This bay already has state, so it is not seeded again. A container
		// gets re-created for reasons that have nothing to do with its data --
		// the daemon restarted, the bay was adopted, an image changed -- and
		// restoring the template over a volume the developer has been working
		// against all afternoon would destroy their work to save two seconds.
		if e.volumeExists(ctx, vol) {
			restore = false
			st.Existing = true
		}
		if err := e.ensureVolume(ctx, name, vol); err != nil {
			return nil, err
		}
		st.Mounts = append(st.Mounts, mount.Mount{
			Type: mount.TypeVolume, Source: vol, Target: dir,
		})
	}

	if restore {
		for _, dir := range dirs {
			if err := e.copyVolume(ctx, st.Templates[dir], st.Volumes[dir]); err != nil {
				return nil, fmt.Errorf("%s: restoring the seeded state: %w", name, err)
			}
		}
		st.Restored = true
		e.Log("  %s: restored seeded state (%s)", name, hash)
	}
	return st, nil
}

// captureSeeds records the template for any seeded service that just built its
// state the slow way. Called once a bay is up and its seeding steps have
// completed, because that is the only moment devbay knows the state is the one
// the application actually runs against.
func (e *Engine) captureSeeds(ctx context.Context) {
	for _, name := range sortedServices(e.m) {
		st := e.seeds[name]
		if st == nil || st.Restored || st.Existing {
			// Nothing to capture from a bay that has been running: the state
			// has diverged from what the seeding steps produce, and a template
			// taken from it would seed every future bay with one developer's
			// afternoon.
			continue
		}
		if err := e.captureSeed(ctx, name, st); err != nil {
			// Never fatal. A bay that works but did not leave a cache behind
			// is a slow bay; a bay that refuses to exist because a cache could
			// not be written is a broken tool.
			e.Log("  %s: could not capture the seeded state: %v", name, err)
		}
	}
}

func (e *Engine) captureSeed(ctx context.Context, name string, st *seedState) error {
	id, err := e.containerID(ctx, name)
	if err != nil {
		return err
	}
	// Frozen for the copy. A datastore writing while its files are read
	// produces a torn page, and while every one of these engines recovers from
	// that on start -- it is the same state a power cut leaves -- there is no
	// reason to hand it one.
	if _, err := e.cli.ContainerPause(ctx, id, client.ContainerPauseOptions{}); err != nil {
		return fmt.Errorf("pausing for the snapshot: %w", err)
	}
	defer func() {
		if _, err := e.cli.ContainerUnpause(context.WithoutCancel(ctx), id, client.ContainerUnpauseOptions{}); err != nil {
			e.Log("  %s: could not resume after the snapshot: %v", name, err)
		}
	}()

	for dir, tmpl := range st.Templates {
		if err := e.createSeedVolume(ctx, name, tmpl); err != nil {
			return err
		}
		if err := e.copyVolume(ctx, st.Volumes[dir], tmpl); err != nil {
			// A half-copied template is worse than none: the next bay would
			// restore it and start a database with a truncated data directory.
			_, _ = e.cli.VolumeRemove(context.WithoutCancel(ctx), tmpl, client.VolumeRemoveOptions{Force: true})
			return err
		}
	}
	e.Log("  %s: captured the seeded state; the next bay of this project skips it (%s)", name, st.Hash)
	return nil
}

// createSeedVolume makes a template volume, labelled by project and not by bay.
func (e *Engine) createSeedVolume(ctx context.Context, service, name string) error {
	l := map[string]string{
		LabelManaged: "1",
		LabelProject: e.m.Project,
		LabelService: service,
		LabelSeed:    "1",
	}
	_, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: l})
	if err != nil {
		return fmt.Errorf("creating template volume %s: %w", name, err)
	}
	return nil
}

func (e *Engine) volumeExists(ctx context.Context, name string) bool {
	_, err := e.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	return err == nil
}

// copyVolume copies one volume's contents into another.
//
// Through a container because a named volume has no path on the host worth
// knowing: on macOS and on any Docker context that is not a local Linux
// daemon, /var/lib/docker is inside a VM. The one portable way to read a
// volume is to mount it.
func (e *Engine) copyVolume(ctx context.Context, from, to string) error {
	if err := e.ensureCopyImage(ctx); err != nil {
		return err
	}
	res, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: reclaimImage,
			// -a preserves ownership and modes, which a datastore checks:
			// postgres refuses to start if its data directory is group
			// readable or owned by the wrong user.
			Cmd:    []string{"sh", "-c", "cp -a /from/. /to/"},
			Labels: e.labels(""),
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: from, Target: "/from", ReadOnly: true},
				{Type: mount.TypeVolume, Source: to, Target: "/to"},
			},
			NetworkMode: "none",
		},
	})
	if err != nil {
		return fmt.Errorf("creating the copy container: %w", err)
	}
	defer func() {
		_, _ = e.cli.ContainerRemove(context.WithoutCancel(ctx), res.ID,
			client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := e.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting the copy: %w", err)
	}
	wait := e.cli.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		return err
	case st := <-wait.Result:
		if st.StatusCode != 0 {
			return fmt.Errorf("copying %s to %s exited %d", from, to, st.StatusCode)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// reclaimImage is the smallest image with a shell and cp. Shared with the
// ownership reclaim in internal/bay for the same reason: one more image pulled
// on a clean machine is one more thing that can fail before a bay exists.
const reclaimImage = "busybox:stable"

func (e *Engine) ensureCopyImage(ctx context.Context) error {
	found, err := e.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", reclaimImage),
	})
	if err == nil && len(found.Items) > 0 {
		return nil
	}
	resp, err := e.cli.ImagePull(ctx, reclaimImage, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", reclaimImage, err)
	}
	defer resp.Close()
	return resp.Wait(ctx)
}

// seedRestored reports whether a step is a seeding step this bay can skip,
// which is where the time is actually saved.
func (e *Engine) seedRestored(service string) bool {
	for owner, st := range e.seeds {
		if st == nil || !st.Restored {
			continue
		}
		s := e.m.Services[owner]
		if s == nil || s.Seed == nil {
			continue
		}
		for _, step := range s.Seed.After {
			if step == service {
				return true
			}
		}
	}
	return false
}

// removeSeedTemplates drops this project's templates when the last bay goes.
//
// HC6: teardown reverses creation, and a seeded database left on the machine
// after the last bay was removed is precisely the leaked fork that rule names.
// The cache is worth having for as long as a bay exists and no longer.
func (e *Engine) removeSeedTemplates(ctx context.Context) error {
	others, err := e.cli.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", LabelManaged+"=1").
			Add("label", LabelProject+"="+e.m.Project),
	})
	if err != nil {
		return err
	}
	for _, c := range others.Items {
		if c.Labels[LabelBay] != e.bay {
			return nil // another bay of this project still wants the cache
		}
	}

	vols, err := e.cli.VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).
			Add("label", LabelSeed+"=1").
			Add("label", LabelProject+"="+e.m.Project),
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range vols.Items {
		if _, err := e.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
			errs = append(errs, fmt.Errorf("removing template volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}

// slug turns a path into something a volume name can hold.
func slug(path string) string {
	s := strings.Trim(path, "/")
	s = strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(s)
	if s == "" {
		return "root"
	}
	return s
}

func sortedServices(m *manifest.Manifest) []string {
	out := make([]string, 0, len(m.Services))
	for name := range m.Services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
