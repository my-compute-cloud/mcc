package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/agent"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
)

// controlSocket is where a running agent answers its owner: what it is doing, and whether to
// drain. Only the agent's own account can open it.
func controlSocket(dir string) string { return filepath.Join(dir, "agent.sock") }

// serveControl serves the agent's control socket until ctx ends.
func serveControl(ctx context.Context, path string, h http.Handler) error {
	_ = os.Remove(path) // the lock on the state directory says no other agent is serving it
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
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

// errNoAgent is returned when no agent is running to be asked.
var errNoAgent = errors.New("the agent is not running on this host")

// askAgent sends one request to the agent running with this state directory.
func askAgent(ctx context.Context, dir, method, path string) (agent.Activity, error) {
	var out agent.Activity
	socket := controlSocket(dir)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://mcc"+path, nil)
	if err != nil {
		return out, err
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errNoAgent
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("the agent answered %s; it may be an older version", res.Status)
	}
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// describeWork says in a few words what an agent has in hand.
func describeWork(a agent.Activity) string {
	if a.Idle {
		return "nothing is running"
	}
	n := len(a.Work)
	out := fmt.Sprintf("%d running", n)
	if n == 0 {
		out = "no work running"
	}
	if len(a.Preparing) > 0 {
		out += fmt.Sprintf(", %d image%s being fetched", len(a.Preparing), yes(len(a.Preparing) == 1, "", "s"))
	}
	return out
}

// drain is `mcc drain` and `mcc resume`: the owner tells the agent on this host to take no
// new work, or to take it again.
func drain(verb string, args []string) error {
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	wait := fs.Bool("wait", false, "return once nothing is running")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	ctx, stop := interruptible()
	defer stop()
	act, err := askAgent(ctx, dir, http.MethodPost, "/v1/"+verb)
	if err != nil {
		return err
	}
	if verb == "drain" && *wait && !act.Idle {
		fmt.Fprintf(os.Stderr, "Draining: %s. Waiting for it to finish...\n", describeWork(act))
		for !act.Idle {
			select {
			case <-ctx.Done():
				return errors.New("stopped waiting; the host is still draining. `mcc resume` lets it take work again")
			case <-time.After(time.Second):
			}
			if act, err = askAgent(ctx, dir, http.MethodGet, "/v1/status"); err != nil {
				return err
			}
		}
	}
	if *asJSON {
		b, _ := json.MarshalIndent(act, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	switch {
	case verb == "resume" && act.Draining == agent.DrainUpgrade:
		fmt.Println("This host is still draining, because a newer agent is installed and waits for it to be idle.")
	case verb == "resume":
		fmt.Println("This host takes work again.")
	case act.Idle:
		fmt.Println("This host is drained: it takes no new work and nothing is running. `mcc resume` undoes it.")
	default:
		fmt.Printf("This host takes no new work; %s. `mcc drain --wait` waits for that; `mcc resume` undoes it.\n", describeWork(act))
	}
	return nil
}
