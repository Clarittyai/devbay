package bay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// reclaimImage is what runs the chown. Chosen for being tiny and universally
// present rather than for anything it does: the only requirement is a shell
// and a chown.
const reclaimImage = "busybox:stable"

// reclaimOwnership gives a worktree back to the user who owns this process.
//
// A container that writes into the bind-mounted worktree writes as the user it
// runs as, which for most images is root. On Linux that is the host's
// ownership -- there is no mapping layer between the container's uid and the
// filesystem's -- so a build artefact, an installed dependency, or a test
// report left behind by a container is a root-owned file, and `devbay rm`
// fails to delete it after having already destroyed the containers. That is
// the worst possible shape for this failure: the promise is that teardown is
// total, and half a teardown leaves a worktree nothing can remove.
//
// macOS never shows it, because its file sharing layer maps ownership to the
// calling user. The bug therefore existed from the first commit and only
// appeared when CI ran the suite on Linux.
//
// So devbay borrows the one privilege it is already trusted with -- it can
// start containers, and a container can run as root -- to hand the files back.
// Called only after a removal has already failed for lack of permission, so
// the normal path costs nothing.
func (m *Manager) reclaimOwnership(ctx context.Context, path string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("cannot reclaim ownership of %s on this platform", path)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		// Already root: the files are ours and something else is wrong, so say
		// so rather than running a container that would change nothing.
		return fmt.Errorf("running as root, so %s is already owned by this process", path)
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := m.ensureReclaimImage(ctx); err != nil {
		return err
	}

	owner := strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
	m.Log("  reclaiming ownership of %s as %s", path, owner)

	res, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: reclaimImage,
			// argv, not a shell string -- the same rule the manifest enforces
			// applies to devbay's own commands.
			Cmd:  []string{"chown", "-R", owner, "/target"},
			User: "0:0",
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type:   mount.TypeBind,
				Source: path,
				Target: "/target",
			}},
			AutoRemove: false, // removed below, so the exit code can be read first
		},
	})
	if err != nil {
		return fmt.Errorf("creating the reclaim container: %w", err)
	}
	defer func() {
		_, _ = m.cli.ContainerRemove(context.WithoutCancel(ctx), res.ID,
			client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := m.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting the reclaim container: %w", err)
	}

	wait := m.cli.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		return fmt.Errorf("waiting for the reclaim container: %w", err)
	case st := <-wait.Result:
		if st.StatusCode != 0 {
			return fmt.Errorf("chown exited with status %d", st.StatusCode)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (m *Manager) ensureReclaimImage(ctx context.Context) error {
	found, err := m.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", reclaimImage),
	})
	if err == nil && len(found.Items) > 0 {
		return nil
	}
	resp, err := m.cli.ImagePull(ctx, reclaimImage, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", reclaimImage, err)
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pulling %s: %w", reclaimImage, err)
	}
	return nil
}

// EnsureWritable gives a bay's worktree back to the developer if a container
// has taken it.
//
// This is about the primary activity, not about cleanup. Containers write into
// the bind-mounted worktree as whatever user they run as, which for most
// images is root, and on Linux that is the filesystem's ownership -- so after
// a bay boots, the developer can find their own source tree read-only. Editing
// is the thing devbay exists to let you do in parallel; a bay you cannot edit
// is not a working bay.
//
// Checked with a write rather than by inspecting modes, because the question is
// exactly "can this process write here" and permissions, ownership, ACLs and
// the platform all bear on the answer. Costs a stat and a create when nothing
// is wrong, which is the common case and the one that must stay cheap.
func (m *Manager) EnsureWritable(ctx context.Context, worktree string) {
	if worktree == "" || writable(worktree) {
		return
	}
	m.Log("bay: a container has taken ownership of %s; taking it back", worktree)
	if err := m.reclaimOwnership(ctx, worktree); err != nil {
		m.Log("bay: could not restore write access to %s: %v", worktree, err)
	}
}

// writable reports whether this process can create a file in a directory.
func writable(dir string) bool {
	probe := filepath.Join(dir, ".devbay-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return os.IsExist(err) // someone else's probe: the directory is writable
	}
	f.Close()
	_ = os.Remove(probe)
	return true
}
