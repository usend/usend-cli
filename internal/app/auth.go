package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	usend "usend.et/sdk/go"
)

// checkKey makes a cheap authenticated call and reports the key's permission.
// GET /domains needs full access; a valid sending-only key gets restricted_api_key.
func (a *app) checkKey() (string, error) {
	_, err := a.call(http.MethodGet, "/domains?limit=1", nil)
	if err == nil {
		return "full_access", nil
	}
	var apiErr *usend.Error
	if errors.As(err, &apiErr) && apiErr.Name == "restricted_api_key" {
		return "sending_access", nil
	}
	return "", err
}

func permissionLabel(p string) string {
	switch p {
	case "full_access":
		return "full access"
	case "sending_access":
		return "sending access only"
	}
	return "unknown"
}

func cmdLogin(a *app, args []string) error {
	const usage = "usend login [--key us_...] [--profile name] [--base-url url]"
	fs := a.newFlags("login", usage, "Check an API key and save it for this computer.\n\nWithout --key you are asked for it (the input is hidden). Create keys in the dashboard under API keys.")
	key := fs.String("key", "", "the API key (prefer the prompt: flags end up in shell history)")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	k := strings.TrimSpace(*key)
	if k == "" {
		if a.env.Stdin == nil {
			return usagef("give the key with --key")
		}
		k, err = a.readSecret("API key: ")
		if err != nil {
			return err
		}
	}
	if k == "" {
		return usagef("no API key given")
	}
	if !strings.HasPrefix(k, "us_") && !strings.HasPrefix(k, "re_") {
		fmt.Fprintln(a.env.Stderr, "Warning: uSend API keys start with us_; checking it anyway.")
	}

	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	name := a.profile
	if name == "" {
		name = defaultProfile
	}
	baseURL := a.baseURL
	if baseURL == "" {
		baseURL = strings.TrimSpace(a.env.Getenv("USEND_BASE_URL"))
	}
	if baseURL == "" {
		if p := cfg.Profiles[name]; p != nil {
			baseURL = p.BaseURL
		}
	}
	if baseURL == "" {
		baseURL = usend.DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	// Check this key, not whatever the environment or config would pick.
	a.creds = &credentials{APIKey: k, KeySource: "login", Profile: name, BaseURL: baseURL, ConfigPath: path}
	perm, err := a.checkKey()
	if err != nil {
		return err
	}
	p := &Profile{APIKey: k, Permission: perm}
	if baseURL != usend.DefaultBaseURL {
		p.BaseURL = baseURL
	}
	cfg.Profiles[name] = p
	cfg.CurrentProfile = name
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	res := map[string]any{"profile": name, "key": maskKey(k), "permission": perm, "base_url": baseURL, "config": path}
	if a.wantJSON() {
		return printValue(a.env.Stdout, res)
	}
	fmt.Fprintf(a.env.Stdout, "Logged in: key %s (%s) saved as profile %q.\nAPI: %s\nConfig: %s\n", maskKey(k), permissionLabel(perm), name, baseURL, path)
	if perm == "sending_access" {
		fmt.Fprintln(a.env.Stdout, "This key can send email only; listing emails, domains and the rest needs a full-access key.")
	}
	if a.env.Getenv("USEND_API_KEY") != "" {
		fmt.Fprintln(a.env.Stdout, "Note: USEND_API_KEY is set and is used instead of the saved key unless you pass --profile.")
	}
	return nil
}

func cmdLogout(a *app, args []string) error {
	const usage = "usend logout [--profile name] [--all]"
	fs := a.newFlags("logout", usage, "Remove a saved API key from this computer (the key itself stays valid; delete it with usend api-keys delete or in the dashboard).")
	all := fs.Bool("all", false, "remove every saved profile")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	var removed []string
	if *all {
		for n := range cfg.Profiles {
			removed = append(removed, n)
		}
		cfg.Profiles = map[string]*Profile{}
		cfg.CurrentProfile = ""
	} else {
		name := a.profile
		if name == "" {
			name = cfg.CurrentProfile
		}
		if name == "" {
			name = defaultProfile
		}
		if _, ok := cfg.Profiles[name]; !ok {
			return fmt.Errorf("no saved profile %q", name)
		}
		delete(cfg.Profiles, name)
		removed = []string{name}
		if cfg.CurrentProfile == name {
			cfg.CurrentProfile = ""
			for n := range cfg.Profiles {
				if n == defaultProfile || cfg.CurrentProfile == "" {
					cfg.CurrentProfile = n
				}
			}
		}
	}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	if a.wantJSON() {
		return printValue(a.env.Stdout, map[string]any{"removed": removed, "current_profile": cfg.CurrentProfile})
	}
	if len(removed) == 0 {
		fmt.Fprintln(a.env.Stdout, "No saved profiles.")
		return nil
	}
	fmt.Fprintf(a.env.Stdout, "Removed profile %s.\n", strings.Join(removed, ", "))
	if cfg.CurrentProfile != "" {
		fmt.Fprintf(a.env.Stdout, "Current profile is now %q.\n", cfg.CurrentProfile)
	}
	return nil
}

func cmdWhoami(a *app, args []string) error {
	const usage = "usend whoami"
	fs := a.newFlags("whoami", usage, "Show which API key, profile and API this computer uses (the key is masked). Use usend doctor to test the key.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	c, err := a.credentials()
	if err != nil {
		return err
	}
	if c.APIKey == "" {
		return errors.New("not logged in: run `usend login` or set USEND_API_KEY")
	}
	profile := c.Profile
	if c.KeySource == "USEND_API_KEY" {
		profile = ""
	}
	if a.wantJSON() {
		return printValue(a.env.Stdout, map[string]any{
			"key": maskKey(c.APIKey), "key_source": c.KeySource, "profile": profile,
			"permission": c.Permission, "base_url": c.BaseURL, "base_url_source": c.URLSource, "config": c.ConfigPath,
		})
	}
	perm := ""
	if c.Permission != "" {
		perm = permissionLabel(c.Permission)
	}
	fields(a.env.Stdout, [][2]string{
		{"Key", maskKey(c.APIKey)},
		{"From", c.KeySource},
		{"Permission", perm},
		{"API", c.BaseURL + " (" + c.URLSource + ")"},
		{"Config", c.ConfigPath},
	})
	return nil
}

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
}

func cmdDoctor(a *app, args []string) error {
	const usage = "usend doctor"
	fs := a.newFlags("doctor", usage, "Check the configuration, the network path to the API and the API key.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	var checks []check
	add := func(name, status, detail string) { checks = append(checks, check{name, status, detail}) }

	// 1. Config file.
	c, cerr := a.credentials()
	path, _ := a.configPath()
	if cerr != nil {
		add("Config", "fail", a.redact(cerr.Error()))
	} else if fi, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			add("Config", "ok", "no config file yet ("+path+")")
		} else {
			add("Config", "fail", err.Error())
		}
	} else if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		add("Config", "warn", fmt.Sprintf("%s is readable by other users (mode %o); run: chmod 600 %s", path, fi.Mode().Perm(), path))
	} else {
		add("Config", "ok", path)
	}

	// 2. API key.
	haveKey := false
	if c != nil {
		switch {
		case c.APIKey == "":
			add("API key", "fail", "none: run `usend login` or set USEND_API_KEY")
		case !strings.HasPrefix(c.APIKey, "us_") && !strings.HasPrefix(c.APIKey, "re_"):
			add("API key", "warn", fmt.Sprintf("%s from %s does not look like a uSend key (us_...)", maskKey(c.APIKey), c.KeySource))
			haveKey = true
		default:
			add("API key", "ok", fmt.Sprintf("%s from %s", maskKey(c.APIKey), c.KeySource))
			haveKey = true
		}
	}

	// 3. Base URL and network.
	reachable := false
	if c != nil {
		u, err := url.Parse(c.BaseURL)
		switch {
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "":
			add("API URL", "fail", fmt.Sprintf("%q is not an http(s) URL", c.BaseURL))
		default:
			if u.Scheme == "http" && !isLocal(u.Hostname()) {
				add("API URL", "warn", c.BaseURL+" is not HTTPS: the key travels unencrypted")
			} else {
				add("API URL", "ok", c.BaseURL+" ("+c.URLSource+")")
			}
			start := time.Now()
			req, _ := http.NewRequestWithContext(a.ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
			req.Header.Set("User-Agent", userAgent())
			resp, err := a.env.HTTPClient.Do(req)
			if err != nil {
				add("Network", "fail", "cannot reach "+u.Host+": "+shortNetErr(err))
			} else {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				ms := time.Since(start).Milliseconds()
				if resp.StatusCode == http.StatusOK {
					add("Network", "ok", fmt.Sprintf("%s answered in %d ms", u.Host, ms))
				} else {
					add("Network", "warn", fmt.Sprintf("%s/healthz answered HTTP %d in %d ms", u.Host, resp.StatusCode, ms))
				}
				reachable = true
			}
		}
	}

	// 4. Key validity.
	switch {
	case !haveKey:
		add("Authentication", "skip", "no API key")
	case !reachable:
		add("Authentication", "skip", "API not reachable")
	default:
		perm, err := a.checkKey()
		var apiErr *usend.Error
		switch {
		case err == nil:
			add("Authentication", "ok", "key accepted ("+permissionLabel(perm)+")")
		case errors.As(err, &apiErr):
			add("Authentication", "fail", fmt.Sprintf("%s (%s, HTTP %d)", apiErr.Message, apiErr.Name, apiErr.StatusCode))
		default:
			add("Authentication", "fail", a.redact(shortNetErr(err)))
		}
	}

	failed := false
	for _, ch := range checks {
		if ch.Status == "fail" {
			failed = true
		}
	}
	if a.wantJSON() {
		if err := printValue(a.env.Stdout, map[string]any{"ok": !failed, "checks": checks}); err != nil {
			return err
		}
	} else {
		w := a.env.Stdout
		for _, ch := range checks {
			fmt.Fprintf(w, "  %-5s %-15s %s\n", strings.ToUpper(ch.Status), ch.Name, ch.Detail)
		}
		if failed {
			fmt.Fprintln(w, "\nSome checks failed.")
		} else {
			fmt.Fprintln(w, "\nAll good.")
		}
	}
	if failed {
		return errSilent
	}
	return nil
}

// errSilent exits 1 without another message (the command already reported).
var errSilent = errors.New("")

func isLocal(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

func shortNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func cmdVersion(a *app, args []string) error {
	const usage = "usend version"
	fs := a.newFlags("version", usage, "Print the CLI version.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	if a.wantJSON() {
		return printValue(a.env.Stdout, map[string]any{
			"version": Version, "sdk": usend.Version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		})
	}
	fmt.Fprintf(a.env.Stdout, "usend %s (Go SDK %s, %s, %s/%s)\n", Version, usend.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}
