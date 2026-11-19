package tasklog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// The output is read over HTTP, so that anything can read it and a broken read can pick up
// where it stopped:
//
//	GET /v1/tasks/<id>                          the run's record, as JSON
//	GET /v1/tasks/<id>/output?offset=N&follow=1 its output from byte N on, as it is written
//
// A caller shows the secret whose hash it gave when it asked for the task, as a bearer token.
// The output response ends with trailers that say how the run ended.
const (
	HeaderState     = "X-Mcc-Task-State" // running or ended, when the response began
	TrailerExitCode = "X-Mcc-Exit-Code"
	TrailerCode     = "X-Mcc-Code"
	TrailerTrunc    = "X-Mcc-Truncated"
)

// Handler serves the store. With ownerOnly, a caller that shows no secret is let in: that is
// for a listener only the host's owner can reach, who can read the files anyway. Without it,
// only the secret whose hash came with the task opens that task's output.
func (s *Store) Handler(ownerOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/v1/tasks/")
		if !ok || r.Method != http.MethodGet {
			httpError(w, http.StatusNotFound, "not found")
			return
		}
		id, what, _ := strings.Cut(rest, "/")
		m, err := s.Meta(id)
		secret, hasSecret := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// The same answer for a task that is not here as for a wrong secret, so that asking does
		// not reveal which tasks ran on this host.
		if err != nil || (hasSecret && !m.Allows(secret)) || (!hasSecret && !ownerOnly) {
			if err != nil && ownerOnly && !hasSecret {
				httpError(w, http.StatusNotFound, ErrNotFound.Error())
				return
			}
			httpError(w, http.StatusUnauthorized, "no output for that task and secret on this host")
			return
		}
		switch what {
		case "":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Meta
				SecretHash string `json:"secret_hash,omitempty"` // never sent
				Size       int64  `json:"size"`
			}{Meta: m, Size: s.Size(id)})
		case "output":
			s.serveOutput(w, r, m)
		default:
			httpError(w, http.StatusNotFound, "not found")
		}
	})
}

func (s *Store) serveOutput(w http.ResponseWriter, r *http.Request, m Meta) {
	q := r.URL.Query()
	var offset int64
	if v := q.Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, "offset must be a whole number of bytes")
			return
		}
		offset = n
	}
	if offset > s.Size(m.ID) {
		httpError(w, http.StatusRequestedRangeNotSatisfiable, "offset is past the end of the output")
		return
	}
	state := "ended"
	if m.Running() {
		state = "running"
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(HeaderState, state)
	h.Set("Trailer", strings.Join([]string{TrailerExitCode, TrailerCode, TrailerTrunc}, ", "))
	w.WriteHeader(http.StatusOK)
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	flush()
	end, err := s.Copy(w, m.ID, offset, q.Get("follow") == "1", r.Context().Done(), flush)
	if err != nil {
		return
	}
	if end.ExitCode != nil {
		h.Set(TrailerExitCode, strconv.Itoa(*end.ExitCode))
	}
	if end.Code != "" {
		h.Set(TrailerCode, end.Code)
	}
	if end.Truncated {
		h.Set(TrailerTrunc, "true")
	}
}

func httpError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ServeUnix serves the store on a Unix socket that only the agent's own account can open,
// until ctx ends. Whoever can open it is the host's owner, so it asks for no secret.
func (s *Store) ServeUnix(ctx context.Context, path string) error {
	// A socket left by an agent that did not stop cleanly would refuse the new one.
	if _, err := net.Dial("unix", path); err == nil {
		return fmt.Errorf("another agent is already serving %s", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.Handler(true)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = os.Remove(path)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// End is how a run ended, as a reader learns it from the end of the output.
type End struct {
	// Running is true if the output stopped while the task was still running: the reader did
	// not ask to follow it.
	Running   bool
	ExitCode  *int
	Code      string
	Truncated bool
	// Read is how many bytes were written to the reader's writer.
	Read int64
}

// ReadUnix copies a task's output from an agent's socket to w. secret may be empty on the
// owner's socket.
func ReadUnix(ctx context.Context, socket, id, secret string, offset int64, follow bool, w io.Writer) (End, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	defer client.CloseIdleConnections()
	return Read(ctx, client, "http://mcc", id, secret, offset, follow, w)
}

// Read copies a task's output from an agent at base to w.
func Read(ctx context.Context, client *http.Client, base, id, secret string, offset int64, follow bool, w io.Writer) (End, error) {
	var end End
	if !idPattern.MatchString(id) {
		return end, ErrNotFound
	}
	u := fmt.Sprintf("%s/v1/tasks/%s/output?offset=%d", base, id, offset)
	if follow {
		u += "&follow=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return end, err
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := client.Do(req)
	if err != nil {
		return end, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		if body.Error == "" {
			body.Error = res.Status
		}
		return end, errors.New(body.Error)
	}
	end.Read, err = io.Copy(w, res.Body)
	if err != nil {
		return end, err
	}
	// Trailers are there once the body has been read to its end.
	if v := res.Trailer.Get(TrailerExitCode); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			end.ExitCode = &n
		}
	}
	end.Code = res.Trailer.Get(TrailerCode)
	end.Truncated = res.Trailer.Get(TrailerTrunc) == "true"
	end.Running = end.ExitCode == nil && end.Code == "" && res.Header.Get(HeaderState) == "running" && !follow
	return end, nil
}
