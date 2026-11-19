package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The most of one task's output that is sent to be kept; the control plane takes no more.
const maxUpload = 64 << 20

// limitedBuffer keeps the first max bytes written to it and notes if there were more.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.over = true
		b.Buffer.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// uploadOutput sends what a finished task printed to the control plane, to be kept there for
// as long as the tenant's plan keeps it. Unlike everything else about output, this hands the
// control plane something it can read, so it happens only when the tenant's plan asks for it
// and this host's owner has said yes (task_output.upload).
func (a *Agent) uploadOutput(taskID string, lg *slog.Logger) {
	store := a.o.TaskOutput
	buf := &limitedBuffer{max: maxUpload}
	meta, err := store.Copy(buf, taskID, 0, false, nil, nil)
	if err != nil {
		lg.Warn("the task's output was not uploaded", "err", err)
		return
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	url := strings.TrimSuffix(a.o.ServerURL, "/") + "/agent/v1/tasks/" + taskID + "/output"
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		ts := strconv.FormatInt(a.o.Now().Unix(), 10)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			last = err
			break
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-MCC-Host", a.o.HostID)
		req.Header.Set("X-MCC-Timestamp", ts)
		req.Header.Set("X-MCC-Truncated", strconv.FormatBool(meta.Truncated || buf.over))
		req.Header.Set("X-MCC-Signature", a.o.Identity.SignUpload(ts, a.o.HostID, taskID, hex.EncodeToString(sum[:])))
		res, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		if res.StatusCode == http.StatusNoContent {
			lg.Info("the task's output was uploaded to be kept", "bytes", len(body))
			return
		}
		last = fmt.Errorf("%s %s", res.Status, strings.TrimSpace(string(msg)))
		// The control plane's no is final; only its failures are worth another try.
		if res.StatusCode < 500 {
			break
		}
	}
	lg.Warn("the task's output was not uploaded", "err", last)
}
