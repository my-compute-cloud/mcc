package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/my-compute-cloud/mcc/agent/internal/api"
	"github.com/my-compute-cloud/mcc/agent/internal/config"
)

// auth is `mcc auth ...`: signing a person in, as opposed to enrolling a host.
func auth(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mcc auth login [--server URL] [--read-only] [--no-browser] | logout | status | token")
	}
	switch args[0] {
	case "login":
		return authLogin(args[1:])
	case "logout":
		return authLogout()
	case "status":
		return authStatus(args[1:])
	case "token":
		c, err := api.LoadCredentials()
		if err != nil {
			return err
		}
		fmt.Println(c.Token)
		return nil
	}
	return fmt.Errorf("unknown: mcc auth %s", args[0])
}

// defaultServer is where to sign in when not told: where this host is enrolled, if it is,
// since that is the control plane its owner uses.
func defaultServer() string {
	if s := os.Getenv("MCC_SERVER_URL"); s != "" {
		return s
	}
	if dir, err := config.Dir(); err == nil {
		if enrolled, err := enrollments(dir); err == nil && len(enrolled) > 0 && enrolled[0].State.ServerURL != "" {
			return enrolled[0].State.ServerURL
		}
	}
	return api.DefaultServer
}

func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func authLogin(args []string) error {
	fs := flag.NewFlagSet("auth login", flag.ExitOnError)
	server := fs.String("server", defaultServer(), "control plane URL")
	readOnly := fs.Bool("read-only", false, "ask for a sign-in that can see everything and change nothing")
	noBrowser := fs.Bool("no-browser", false, "print the address instead of opening a browser")
	_ = fs.Parse(args)
	ctx, stop := interruptible()
	defer stop()

	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	*server = strings.TrimSuffix(*server, "/")
	login, err := api.StartLogin(ctx, *server, host, *readOnly)
	if err != nil {
		return fmt.Errorf("starting the sign-in: %w", err)
	}
	// Everything a person needs is on stderr, so that stdout stays empty for scripts.
	fmt.Fprintf(os.Stderr, "To sign in, open this page and approve the code %s:\n\n  %s\n\n", login.UserCode, login.URLWithCode)
	if !*noBrowser && os.Getenv("MCC_NO_BROWSER") == "" && mayOpen(login.URLWithCode, *server) && openBrowser(login.URLWithCode) == nil {
		fmt.Fprintln(os.Stderr, "It has been opened in your browser.")
	}
	fmt.Fprintf(os.Stderr, "Waiting for your answer (up to %d minutes)...\n", login.ExpiresIn/60)
	creds, err := api.AwaitLogin(ctx, *server, login)
	if err != nil {
		return err
	}
	if err := api.SaveCredentials(creds); err != nil {
		return err
	}
	what := ""
	if *readOnly {
		what = ", read-only"
	}
	fmt.Fprintf(os.Stderr, "Signed in as %s%s.\n", creds.User, what)
	return nil
}

// mayOpen reports whether a page the server named is one to hand to the desktop, which opens
// whatever it is given: a file, or another program's address. Only a web page over TLS is,
// or one on this machine when the server itself is spoken to without TLS.
func mayOpen(page, server string) bool {
	u, err := url.Parse(page)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	s, err := url.Parse(server)
	return err == nil && u.Scheme == "http" && s.Scheme == "http" && loopback(u.Hostname())
}

// loopback reports whether a host name or address is this machine.
func loopback(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// openBrowser asks the desktop to show a page. It fails where there is no desktop to ask.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("/usr/bin/open", url)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no display")
		}
		cmd = exec.Command("xdg-open", url)
	default:
		return errors.New("unsupported")
	}
	return cmd.Start()
}

func authLogout() error {
	c, err := api.LoadCredentials()
	if errors.Is(err, api.ErrSignedOut) {
		fmt.Fprintln(os.Stderr, "Not signed in.")
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The token is ended on the control plane too, so that a copy of it is worth nothing.
	client := &api.Client{Server: c.Server, Token: c.Token}
	_, err = client.Do(ctx, http.MethodDelete, "/api/v1/api-tokens/current", nil, nil)
	if err != nil && !api.IsStatus(err, http.StatusUnauthorized) {
		return fmt.Errorf("the token could not be revoked, so it was kept: %w", err)
	}
	if os.Getenv("MCC_API_TOKEN") != "" {
		fmt.Fprintln(os.Stderr, "The token in MCC_API_TOKEN has been revoked; unset it.")
		return nil
	}
	if err := api.DeleteCredentials(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Signed out.")
	return nil
}

// whoAmI is what GET /api/v1/me answers.
type whoAmI struct {
	Tenant struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"tenant"`
	Plan struct {
		Name      string `json:"name"`
		Hosts     int    `json:"hosts"`
		HostsUsed int    `json:"hosts_used"`
	} `json:"plan"`
	Token struct {
		Name      string     `json:"name"`
		Kind      string     `json:"kind"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expires_at"`
	} `json:"token"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
}

func authStatus(args []string) error {
	fs := flag.NewFlagSet("auth status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	c, err := api.New()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if *asJSON {
		return printRaw(ctx, c, "/api/v1/me")
	}
	var me whoAmI
	if _, err := c.Do(ctx, http.MethodGet, "/api/v1/me", nil, &me); err != nil {
		return err
	}
	who := "a token with no person behind it"
	if me.User != nil {
		who = me.User.Login
	}
	fmt.Printf("Signed in to %s as %s\n", c.Server, who)
	fmt.Printf("  Account  %s (%s plan, %d of %d hosts)\n", me.Tenant.Name, me.Plan.Name, me.Plan.HostsUsed, me.Plan.Hosts)
	may := "everything"
	if me.Token.Scopes != nil {
		may = strings.Join(me.Token.Scopes, ", ")
	}
	fmt.Printf("  Token    %s; may do: %s\n", me.Token.Name, may)
	if me.Token.ExpiresAt != nil {
		fmt.Printf("  Expires  %s, or later if it is used before then\n", me.Token.ExpiresAt.Local().Format("2 Jan 2006"))
	}
	return nil
}
