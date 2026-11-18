package tart

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/executor"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// preloadTimeout bounds pulling the owner's list of container images into a base.
const preloadTimeout = 45 * time.Minute

func (t *Tart) preloads(image protocol.ImageSpec) bool {
	return len(t.Preload) > 0 && image.Platform == "linux"
}

// preloadImages boots the VM once and pulls the owner's container images into its Docker, so
// that jobs, which clone it, start their containers and services without pulling.
func (t *Tart) preloadImages(ctx context.Context, vm string) error {
	ctx, cancel := context.WithTimeout(ctx, preloadTimeout)
	defer cancel()
	t.Log.Info("pulling container images into the image", "images", t.Preload)
	started := time.Now()

	defer t.halt(context.WithoutCancel(ctx), vm, 60)
	if err := t.boot(ctx, vm); err != nil {
		return err
	}
	var out bytes.Buffer
	code, err := t.Exec(ctx, executor.Handle{Name: vm}, executor.Command{
		Script: executor.PreloadScript,
		Stdin:  strings.NewReader(strings.Join(t.Preload, "\n") + "\n"),
		Output: &out,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pulling container images in the VM failed: %s", executor.LastLines(out.String(), 5))
	}
	t.Log.Info("container images pulled", "took", time.Since(started).Round(time.Second))
	return nil
}
