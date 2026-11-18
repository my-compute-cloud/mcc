package kvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
)

// preloadTimeout bounds pulling the owner's list of container images into a base.
const preloadTimeout = 45 * time.Minute

// preloadImages boots a base's own disk once, not an overlay of it, and pulls the owner's
// container images into its Docker, so that jobs, whose disks are overlays of it, start their
// containers and services without pulling.
func (k *KVM) preloadImages(ctx context.Context, name, disk string, info baseInfo) error {
	ctx, cancel := context.WithTimeout(ctx, preloadTimeout)
	defer cancel()
	k.Log.Info("pulling container images into the image", "images", k.Preload)
	started := time.Now()

	// Whatever an agent that crashed here left behind.
	k.halt(name, 0)
	_ = os.RemoveAll(k.vmDir(name))
	defer os.RemoveAll(k.vmDir(name))
	halted := false
	defer func() {
		if !halted {
			k.halt(name, 0)
		}
	}()
	if err := k.boot(ctx, bootSpec{name: name, disk: disk, config: info.Config}); err != nil {
		return err
	}
	var out bytes.Buffer
	code, err := k.Exec(ctx, executor.Handle{Name: name}, executor.Command{
		Script: executor.PreloadScript,
		Stdin:  strings.NewReader(strings.Join(k.Preload, "\n") + "\n"),
		Output: &out,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pulling container images in the VM failed: %s", executor.LastLines(out.String(), 5))
	}
	// Jobs are going to run from this disk, so the guest must have shut down by itself.
	halted = true
	if !k.halt(name, 120) {
		return errors.New("the VM did not shut down cleanly after pulling container images")
	}
	k.Log.Info("container images pulled", "took", time.Since(started).Round(time.Second))
	return nil
}
