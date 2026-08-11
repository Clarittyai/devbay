// Package testutil holds helpers shared by devbay's Docker integration tests.
//
// It exists because two things are true on a clean Linux machine and false on
// the developer machine the tests were written on, and both of them look like
// flakiness rather than what they are:
//
//   - An image the test never pulled is not there. Locally every image is in
//     the cache from a previous run, so a test that creates a container from
//     `alpine:3.20` without pulling it works forever and then fails the first
//     time it meets a fresh runner.
//
//   - A container writes as root, and on Linux that is the host's ownership.
//     Anything a container leaves in a bind-mounted directory -- including the
//     empty mountpoint Docker creates when a volume is mounted at a path
//     inside a bind mount -- is root-owned, and `t.TempDir()`'s own cleanup
//     cannot remove it. macOS maps ownership and never shows this.
//
// Both are properties of the environment, so they belong in one place rather
// than being rediscovered per package.
package testutil

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// reclaimImage is what runs the chown: tiny, and needs only a chown.
const reclaimImage = "busybox:stable"

// PullImage makes sure an image is present, pulling it if not.
//
// Call it before creating a container directly. Code paths that go through the
// engine or the proxy pull for themselves; a test that reaches for the Docker
// API on its own has to do the same.
func PullImage(ctx context.Context, t *testing.T, cli *client.Client, ref string) {
	t.Helper()
	found, err := cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", ref),
	})
	if err == nil && len(found.Items) > 0 {
		return
	}
	t.Logf("pulling %s", ref)
	resp, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pulling %s: %v", ref, err)
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		t.Fatalf("pulling %s: %v", ref, err)
	}
}

// ReclaimOnCleanup arranges for dir to be owned by this process again once the
// test finishes.
//
// Call it immediately after creating the directory. Cleanups run last
// registered first, so registering this after `t.TempDir()` means ownership is
// restored before that directory's own removal is attempted -- which is the
// whole point, since otherwise the removal is what fails.
//
// A failure here is logged rather than fatal: the test's actual subject has
// already been decided by this point, and turning "the runner could not tidy
// up" into a red test would hide the result that matters.
func ReclaimOnCleanup(t *testing.T, cli *client.Client, dir string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		// Elsewhere the file sharing layer maps ownership, so there is nothing
		// to reclaim and no reason to spend a container on finding that out.
		return
	}
	t.Cleanup(func() {
		if err := reclaim(cli, dir); err != nil {
			t.Logf("could not reclaim ownership of %s: %v", dir, err)
		}
	})
}

func reclaim(cli *client.Client, dir string) error {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	found, err := cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", reclaimImage),
	})
	if err != nil || len(found.Items) == 0 {
		resp, err := cli.ImagePull(ctx, reclaimImage, client.ImagePullOptions{})
		if err != nil {
			return err
		}
		defer resp.Close()
		if err := resp.Wait(ctx); err != nil {
			return err
		}
	}

	res, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: reclaimImage,
			Cmd:   []string{"chown", "-R", strconv.Itoa(uid) + ":" + strconv.Itoa(gid), "/target"},
			User:  "0:0",
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: "/target"}},
		},
	})
	if err != nil {
		return err
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), res.ID,
			client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return err
	}
	wait := cli.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		return err
	case <-wait.Result:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
