// Package app is the uSend command-line tool. Main runs one invocation against
// an Env, so tests can drive it with fake streams, environment and API server.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	usend "usend.et/sdk/go"
)

// Version is the CLI version. Release builds set it with
// -ldflags "-X usend.et/cli/internal/app.Version=1.0.0"; `go install usend.et/cli/cmd/usend@v1.0.0` sets nothing,
// so then it comes from the module version Go records in the binary.
var Version = "dev"

func init() {
	if Version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
}

// Env is everything an invocation touches outside the process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads an environment variable.
	Getenv func(string) string
	// ConfigDir returns the user config directory (os.UserConfigDir).
	ConfigDir func() (string, error)
	// StdoutTTY reports whether stdout is a terminal: tables there, JSON otherwise.
	StdoutTTY bool
	// StdinTTY reports whether stdin is a terminal (prompts, confirmations).
	StdinTTY bool
	// HTTPClient is used for every request.
	HTTPClient *http.Client
}

// DefaultEnv is the environment of the running process.
func DefaultEnv() *Env {
	return &Env{
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Getenv:     os.Getenv,
		ConfigDir:  os.UserConfigDir,
		StdoutTTY:  isTerminal(os.Stdout),
		StdinTTY:   isTerminal(os.Stdin),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// app is one invocation.
type app struct {
	env *Env
	ctx context.Context

	// Global flags, accepted before the command and by every command.
	jsonOut bool
	profile string
	baseURL string

	client *usend.Client
	creds  *credentials
}

// errHelp means help was printed on request: exit 0.
var errHelp = errors.New("help shown")

// usageError is a mistake on the command line: exit 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

type cmdFunc func(a *app, args []string) error

type command struct {
	name    string
	summary string
	run     cmdFunc
}

func commands() []command {
	return []command{
		{"login", "Save an API key for this computer", cmdLogin},
		{"logout", "Remove a saved API key", cmdLogout},
		{"whoami", "Show the profile, key and API in use", cmdWhoami},
		{"send", "Send an email", cmdSend},
		{"emails", "List, inspect and cancel sent emails", cmdEmails},
		{"domains", "List, add, inspect and verify domains", cmdDomains},
		{"inbound", "Read received emails", cmdInbound},
		{"webhooks", "List, create and delete webhooks", cmdWebhooks},
		{"api-keys", "List, create and delete API keys", cmdAPIKeys},
		{"logs", "Read the API request log", cmdLogs},
		{"doctor", "Check the configuration, network and API key", cmdDoctor},
		{"version", "Print the version", cmdVersion},
	}
}

// Main runs the CLI with args (without the program name) and returns the exit code.
func Main(args []string, env *Env) int {
	if env.HTTPClient == nil {
		env.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	a := &app{env: env, ctx: context.Background()}
	err := a.run(args)
	switch {
	case err == nil, errors.Is(err, errHelp):
		return 0
	case errors.Is(err, errSilent):
		return 1
	default:
		return a.printError(err)
	}
}

func (a *app) run(args []string) error {
	fs := flag.NewFlagSet("usend", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a.globalFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			a.mainUsage(a.env.Stdout)
			return errHelp
		}
		return usagef("%v", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		a.mainUsage(a.env.Stdout)
		return errHelp
	}
	name := rest[0]
	if name == "help" {
		if len(rest) > 1 {
			return a.run([]string{rest[1], "--help"})
		}
		a.mainUsage(a.env.Stdout)
		return errHelp
	}
	for _, c := range commands() {
		if c.name == name {
			return c.run(a, rest[1:])
		}
	}
	return usagef("unknown command %q (run `usend help`)", name)
}

func (a *app) globalFlags(fs *flag.FlagSet) {
	fs.BoolVar(&a.jsonOut, "json", a.jsonOut, "print JSON (the default when stdout is not a terminal)")
	fs.StringVar(&a.profile, "profile", a.profile, "use this saved profile")
	fs.StringVar(&a.baseURL, "base-url", a.baseURL, "API base URL (default $USEND_BASE_URL or "+usend.DefaultBaseURL+")")
}

func (a *app) mainUsage(w io.Writer) {
	fmt.Fprint(w, `usend is the command-line tool for the uSend email API.

Usage:
  usend <command> [subcommand] [flags]

Commands:
`)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Global flags (accepted by every command):
  --json            print JSON (the default when stdout is not a terminal)
  --profile <name>  use this saved profile (see usend login)
  --base-url <url>  API base URL

Environment:
  USEND_API_KEY     API key to use instead of the saved one
  USEND_BASE_URL    API base URL (default `+usend.DefaultBaseURL+`)

Run "usend <command> --help" for a command's flags.
`)
}

// newFlags returns a flag set for a command with the global flags registered.
func (a *app) newFlags(name, usage, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a.globalFlags(fs)
	fs.Usage = func() {
		w := a.env.Stdout
		fmt.Fprintf(w, "%s\n\nUsage:\n  %s\n", summary, usage)
		global := map[string]bool{"json": true, "profile": true, "base-url": true}
		printFlags := func(title string, want bool) {
			first := true
			fs.VisitAll(func(f *flag.Flag) {
				if global[f.Name] != want {
					return
				}
				if first {
					fmt.Fprintf(w, "\n%s:\n", title)
					first = false
				}
				typ, help := flag.UnquoteUsage(f)
				if typ == "value" {
					typ = "string"
				}
				name := "--" + f.Name
				if typ != "" {
					name += " <" + typ + ">"
				}
				def := ""
				if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
					def = fmt.Sprintf(" (default %q)", f.DefValue)
				}
				fmt.Fprintf(w, "  %-28s %s%s\n", name, help, def)
			})
		}
		printFlags("Flags", false)
		printFlags("Global flags", true)
	}
	return fs
}

// parse parses flags anywhere among the arguments and returns the positional ones.
func (a *app) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	// The flag package prints usage itself on errors; print it only for --help.
	show := fs.Usage
	fs.Usage = func() {}
	defer func() { fs.Usage = show }()
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				show()
				return nil, errHelp
			}
			return nil, usagef("%v (run `usend %s --help`)", err, fs.Name())
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// group dispatches "usend <name> <sub> ...".
func (a *app) group(name, summary string, args []string, subs []command) error {
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "%s\n\nUsage:\n  usend %s <subcommand> [flags]\n\nSubcommands:\n", summary, name)
		for _, s := range subs {
			fmt.Fprintf(w, "  %-8s %s\n", s.name, s.summary)
		}
		fmt.Fprintf(w, "\nRun \"usend %s <subcommand> --help\" for its flags.\n", name)
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(a.env.Stdout)
		if len(args) == 0 {
			return usagef("usend %s needs a subcommand", name)
		}
		return errHelp
	}
	for _, s := range subs {
		if s.name == args[0] {
			return s.run(a, args[1:])
		}
	}
	return usagef("unknown subcommand %q for usend %s (run `usend %s --help`)", args[0], name, name)
}

// wantJSON reports whether output should be JSON.
func (a *app) wantJSON() bool { return a.jsonOut || !a.env.StdoutTTY }

// api returns the SDK client for the resolved credentials.
func (a *app) api() (*usend.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	c, err := a.credentials()
	if err != nil {
		return nil, err
	}
	if c.APIKey == "" {
		return nil, errors.New("no API key: run `usend login` or set USEND_API_KEY")
	}
	cl, err := usend.New(c.APIKey,
		usend.WithBaseURL(c.BaseURL),
		usend.WithHTTPClient(a.env.HTTPClient),
		usend.WithMaxRetries(1))
	if err != nil {
		return nil, err
	}
	a.client = cl
	return cl, nil
}

func userAgent() string {
	return fmt.Sprintf("usend-cli/%s usend-go/%s (%s/%s)", Version, usend.Version, runtime.GOOS, runtime.GOARCH)
}

// call makes one API request and returns the raw JSON response.
func (a *app) call(method, path string, body any, opts ...usend.RequestOption) (json.RawMessage, error) {
	cl, err := a.api()
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	opts = append([]usend.RequestOption{usend.WithHeader("User-Agent", userAgent())}, opts...)
	if err := cl.Request(a.ctx, method, path, body, &raw, opts...); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return raw, nil
}

// printError reports err on stderr and returns the exit code.
func (a *app) printError(err error) int {
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(a.env.Stderr, "Error: %s\n", ue.msg)
		return 2
	}
	var apiErr *usend.Error
	if errors.As(err, &apiErr) {
		if a.wantJSON() {
			b, _ := json.Marshal(map[string]any{"statusCode": apiErr.StatusCode, "name": apiErr.Name, "message": apiErr.Message})
			fmt.Fprintln(a.env.Stderr, string(b))
		} else {
			fmt.Fprintf(a.env.Stderr, "Error: %s (%s, HTTP %d)\n", apiErr.Message, apiErr.Name, apiErr.StatusCode)
		}
		return 1
	}
	msg := a.redact(err.Error())
	if a.wantJSON() {
		b, _ := json.Marshal(map[string]any{"name": "cli_error", "message": msg})
		fmt.Fprintln(a.env.Stderr, string(b))
	} else {
		fmt.Fprintf(a.env.Stderr, "Error: %s\n", msg)
	}
	return 1
}

// redact removes the API key in use from a message, should anything echo it.
func (a *app) redact(s string) string {
	if a.creds != nil && len(a.creds.APIKey) >= 8 {
		s = strings.ReplaceAll(s, a.creds.APIKey, maskKey(a.creds.APIKey))
	}
	return s
}

// confirm asks a yes/no question when stdin is a terminal; scripts proceed.
func (a *app) confirm(question string, yes bool) error {
	if yes || !a.env.StdinTTY {
		return nil
	}
	fmt.Fprintf(a.env.Stderr, "%s [y/N] ", question)
	line, _ := readLine(a.env.Stdin)
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("aborted")
}

// readLine reads one line without buffering past it.
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return strings.TrimRight(sb.String(), "\r"), nil
			}
			sb.WriteByte(b[0])
		}
		if err != nil {
			if sb.Len() > 0 && errors.Is(err, io.EOF) {
				return sb.String(), nil
			}
			return sb.String(), err
		}
	}
}

// readSecret prompts for a secret, hiding the input on a Unix terminal.
func (a *app) readSecret(prompt string) (string, error) {
	fmt.Fprint(a.env.Stderr, prompt)
	f, isFile := a.env.Stdin.(*os.File)
	hide := a.env.StdinTTY && isFile && runtime.GOOS != "windows"
	if hide {
		if err := stty(f, "-echo"); err != nil {
			hide = false
		} else {
			defer func() {
				_ = stty(f, "echo")
				fmt.Fprintln(a.env.Stderr)
			}()
		}
	}
	line, err := readLine(a.env.Stdin)
	if err != nil && line == "" {
		return "", errors.New("no API key given")
	}
	return strings.TrimSpace(line), nil
}

func stty(tty *os.File, arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = tty
	return cmd.Run()
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// splitList adds comma-separated values, for flags whose values never contain commas.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
