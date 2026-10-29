package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const testKey = "us_live_0123456789abcdefSECRET"

// fakeAPI is a minimal uSend API: it checks the bearer key and records requests.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	requests []recorded
	// keys maps an accepted key to its permission.
	keys   map[string]string
	routes map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
}

type recorded struct {
	Method, Path, Query string
	Body                []byte
	Header              http.Header
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, keys: map[string]string{testKey: "full_access"}, routes: map[string]func(http.ResponseWriter, *http.Request, []byte){}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func apiError(w http.ResponseWriter, status int, name, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"statusCode": status, "name": name, "message": msg})
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Clone()})
	f.mu.Unlock()
	if r.URL.Path == "/healthz" {
		w.Write([]byte(`{"status":"ok"}`))
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	perm, ok := f.keys[key]
	if !ok {
		apiError(w, 403, "invalid_api_key", "API key is invalid")
		return
	}
	route := r.Method + " " + r.URL.Path
	if perm == "sending_access" && route != "POST /emails" {
		apiError(w, 401, "restricted_api_key", "This API key can only send emails")
		return
	}
	if h, ok := f.routes[route]; ok {
		h(w, r, body)
		return
	}
	apiError(w, 404, "not_found", "Not found")
}

func (f *fakeAPI) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) handle(route, response string) {
	f.routes[route] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(response))
	}
}

type testEnv struct {
	*Env
	vars      map[string]string
	configDir string
	out, err  *bytes.Buffer
}

func newEnv(t *testing.T, baseURL string) *testEnv {
	te := &testEnv{vars: map[string]string{}, configDir: t.TempDir(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if baseURL != "" {
		te.vars["USEND_BASE_URL"] = baseURL
	}
	te.Env = &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     te.out,
		Stderr:     te.err,
		Getenv:     func(k string) string { return te.vars[k] },
		ConfigDir:  func() (string, error) { return te.configDir, nil },
		StdoutTTY:  true,
		HTTPClient: http.DefaultClient,
	}
	return te
}

func (te *testEnv) run(args ...string) int {
	te.out.Reset()
	te.err.Reset()
	return Main(args, te.Env)
}

func (te *testEnv) configFile() string { return filepath.Join(te.configDir, "usend", "config.json") }

func assertNoKey(t *testing.T, te *testEnv) {
	t.Helper()
	if strings.Contains(te.out.String(), testKey) || strings.Contains(te.err.String(), testKey) {
		t.Fatalf("full API key printed:\nstdout: %s\nstderr: %s", te.out, te.err)
	}
}

func TestLoginSavesKeyWith0600(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /domains", `{"object":"list","data":[]}`)
	te := newEnv(t, srv.URL)

	if code := te.run("login", "--key", testKey); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	assertNoKey(t, te)
	if !strings.Contains(te.out.String(), maskKey(testKey)) || !strings.Contains(te.out.String(), "full access") {
		t.Fatalf("unexpected output: %s", te.out)
	}
	if got := f.last(); got.Header.Get("Authorization") != "Bearer "+testKey || !strings.HasPrefix(got.Header.Get("User-Agent"), "usend-cli/") {
		t.Fatalf("bad request headers: %v", got.Header)
	}

	fi, err := os.Stat(te.configFile())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", fi.Mode().Perm())
	}
	var cfg Config
	b, _ := os.ReadFile(te.configFile())
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["default"]
	if cfg.CurrentProfile != "default" || p == nil || p.APIKey != testKey || p.Permission != "full_access" || p.BaseURL != srv.URL {
		t.Fatalf("config = %s", b)
	}

	// The saved key is used without the environment variable.
	f.handle("GET /webhooks", `{"object":"list","data":[]}`)
	if code := te.run("webhooks", "list"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if f.last().Header.Get("Authorization") != "Bearer "+testKey {
		t.Fatal("saved key not used")
	}
}

func TestLoginFromStdin(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /domains", `{"object":"list","data":[]}`)
	te := newEnv(t, srv.URL)
	te.Stdin = strings.NewReader(testKey + "\n")
	if code := te.run("login"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	assertNoKey(t, te)
}

func TestLoginSendingKeyAndProfiles(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /domains", `{"object":"list","data":[]}`)
	sendKey := "us_send_abcdefghijklmnopqrstuv"
	f.keys[sendKey] = "sending_access"
	te := newEnv(t, srv.URL)

	if code := te.run("login", "--key", testKey); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if code := te.run("login", "--key", sendKey, "--profile", "ci"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if !strings.Contains(te.out.String(), "sending access only") {
		t.Fatalf("output: %s", te.out)
	}
	// ci is now current; --profile default switches for one call.
	if code := te.run("whoami", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	var who map[string]any
	json.Unmarshal(te.out.Bytes(), &who)
	if who["profile"] != "ci" || who["permission"] != "sending_access" || who["key"] != maskKey(sendKey) {
		t.Fatalf("whoami = %v", who)
	}
	if code := te.run("--profile", "default", "whoami"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if !strings.Contains(te.out.String(), "profile default") {
		t.Fatalf("whoami: %s", te.out)
	}
	assertNoKey(t, te)

	if code := te.run("logout", "--profile", "ci"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if code := te.run("whoami", "--profile", "ci"); code == 0 {
		t.Fatal("profile ci should be gone")
	}
}

func TestLoginRejectsInvalidKey(t *testing.T) {
	_, srv := newFakeAPI(t)
	te := newEnv(t, srv.URL)
	if code := te.run("login", "--key", "us_wrong_key_1234567890"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(te.err.String(), "API key is invalid") || !strings.Contains(te.err.String(), "invalid_api_key") {
		t.Fatalf("stderr: %s", te.err)
	}
	if _, err := os.Stat(te.configFile()); !os.IsNotExist(err) {
		t.Fatal("config written for an invalid key")
	}
}

func TestEnvKeyAndWhoamiMasks(t *testing.T) {
	_, srv := newFakeAPI(t)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey
	if code := te.run("whoami"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	assertNoKey(t, te)
	out := te.out.String()
	if !strings.Contains(out, "USEND_API_KEY") || !strings.Contains(out, srv.URL) || !strings.Contains(out, "us_liv...CRET") {
		t.Fatalf("whoami: %s", out)
	}
}

func TestSend(t *testing.T) {
	f, srv := newFakeAPI(t)
	var body map[string]any
	f.routes["POST /emails"] = func(w http.ResponseWriter, r *http.Request, b []byte) {
		json.Unmarshal(b, &body)
		w.Write([]byte(`{"id":"0b5a7f1e-0000-4000-8000-000000000001"}`))
	}
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey
	htmlFile := filepath.Join(t.TempDir(), "mail.html")
	os.WriteFile(htmlFile, []byte("<p>Hi Ann</p>"), 0o644)

	code := te.run("send", "--from", "Acme <hi@acme.com>", "--to", "ann@example.com", "--to", "bob@example.com",
		"--subject", "Hello", "--html-file", htmlFile, "--text", "Hi Ann", "--cc", "c@example.com",
		"--reply-to", "help@acme.com", "--tag", "category=welcome", "--scheduled-at", "in 1 hour",
		"--idempotency-key", "abc-1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if !strings.Contains(te.out.String(), "Scheduled email 0b5a7f1e-0000-4000-8000-000000000001") {
		t.Fatalf("stdout: %s", te.out)
	}
	want := map[string]any{
		"from": "Acme <hi@acme.com>", "subject": "Hello", "html": "<p>Hi Ann</p>", "text": "Hi Ann", "scheduled_at": "in 1 hour",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	b, _ := json.Marshal([]any{body["to"], body["cc"], body["reply_to"], body["tags"]})
	if string(b) != `[["ann@example.com","bob@example.com"],["c@example.com"],["help@acme.com"],[{"name":"category","value":"welcome"}]]` {
		t.Errorf("lists = %s", b)
	}
	if f.last().Header.Get("Idempotency-Key") != "abc-1" {
		t.Error("idempotency key not sent")
	}

	// JSON output when stdout is not a terminal.
	te.StdoutTTY = false
	if code := te.run("send", "--from", "hi@acme.com", "--to", "a@b.com", "--subject", "x", "--text", "y"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	var res map[string]any
	if err := json.Unmarshal(te.out.Bytes(), &res); err != nil || res["id"] == nil {
		t.Fatalf("not JSON: %s", te.out)
	}
}

func TestSendUsageErrors(t *testing.T) {
	_, srv := newFakeAPI(t)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey
	for _, args := range [][]string{
		{"send", "--to", "a@b.com", "--text", "x"},
		{"send", "--from", "a@b.com", "--text", "x"},
		{"send", "--from", "a@b.com", "--to", "a@b.com"},
		{"send", "--from", "a@b.com", "--to", "a@b.com", "--text", "x", "--tag", "novalue"},
		{"send", "--bogus"},
	} {
		if code := te.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, te.err)
		}
	}
	if code := te.run("send", "--help"); code != 0 || !strings.Contains(te.out.String(), "--html-file") {
		t.Errorf("send --help: exit %d, %s", code, te.out)
	}
}

const domainJSON = `{
  "object": "domain", "id": "6a1d2c3e-1111-4222-8333-444455556666", "name": "acme.com", "status": "pending",
  "region": "self-hosted", "created_at": "2026-10-20T10:00:00Z", "receiving_enabled": false,
  "records": [
    {"record": "DKIM", "name": "usk3m9q2xa._domainkey", "type": "TXT", "ttl": "Auto", "status": "pending", "value": "p=MIIBIjANBgkq"},
    {"record": "SPF", "name": "send", "type": "CNAME", "ttl": "Auto", "status": "verified", "value": "send.umta.et"},
    {"record": "Receiving", "name": "@", "type": "MX", "ttl": "Auto", "status": "pending", "value": "mx1.usend.et", "priority": 10}
  ],
  "hints": [{"record": "DKIM", "message": "two DKIM keys are published"}]
}`

func TestDomainsGetTable(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /domains/6a1d2c3e-1111-4222-8333-444455556666", domainJSON)
	f.handle("GET /domains", `{"object":"list","data":[`+domainJSON+`]}`)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey

	// By name: resolved through the list.
	if code := te.run("domains", "get", "acme.com"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	out := te.out.String()
	for _, s := range []string{"RECORD", "TYPE", "NAME", "VALUE", "usk3m9q2xa._domainkey", "p=MIIBIjANBgkq", "send.umta.et", "mx1.usend.et", "two DKIM keys", "usend domains verify"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	// Columns line up: every record row starts at the same column as its header.
	lines := strings.Split(out, "\n")
	var header string
	for _, l := range lines {
		if strings.HasPrefix(l, "RECORD") {
			header = l
		}
	}
	col := strings.Index(header, "VALUE")
	for _, l := range lines {
		if strings.HasPrefix(l, "DKIM ") && !strings.HasPrefix(l[col:], "p=MIIB") {
			t.Errorf("value column misaligned:\n%s", out)
		}
	}
}

func TestJSONVersusTable(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /domains", `{"object":"list","data":[`+domainJSON+`]}`)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey

	isJSON := func() bool {
		var v map[string]any
		return json.Unmarshal(te.out.Bytes(), &v) == nil && v["object"] == "list"
	}

	te.StdoutTTY = true
	if code := te.run("domains", "list"); code != 0 || isJSON() || !strings.Contains(te.out.String(), "NAME") {
		t.Fatalf("terminal: want table, got (exit %d) %s", code, te.out)
	}
	if code := te.run("domains", "list", "--json"); code != 0 || !isJSON() {
		t.Fatalf("--json: want JSON, got %s", te.out)
	}
	if code := te.run("--json", "domains", "list"); code != 0 || !isJSON() {
		t.Fatalf("global --json: want JSON, got %s", te.out)
	}
	te.StdoutTTY = false
	if code := te.run("domains", "list"); code != 0 || !isJSON() {
		t.Fatalf("pipe: want JSON, got %s", te.out)
	}
}

func TestAPIErrorIsReported(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.routes["POST /emails"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		apiError(w, 422, "validation_error", "The `from` domain is not verified.")
	}
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey

	if code := te.run("send", "--from", "a@nope.com", "--to", "b@c.com", "--subject", "s", "--text", "t"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := te.err.String(); !strings.Contains(got, "The `from` domain is not verified.") || !strings.Contains(got, "validation_error") || !strings.Contains(got, "422") {
		t.Fatalf("stderr: %s", got)
	}
	if te.out.Len() != 0 {
		t.Fatalf("stdout should be empty: %s", te.out)
	}
	assertNoKey(t, te)

	// JSON mode keeps the API's error shape on stderr.
	te.StdoutTTY = false
	te.run("emails", "get", "missing")
	var e map[string]any
	if err := json.Unmarshal(te.err.Bytes(), &e); err != nil || e["name"] != "not_found" || e["statusCode"] != float64(404) {
		t.Fatalf("stderr: %s", te.err)
	}
}

func TestMissingKey(t *testing.T) {
	_, srv := newFakeAPI(t)
	te := newEnv(t, srv.URL)
	if code := te.run("domains", "list"); code != 1 || !strings.Contains(te.err.String(), "usend login") {
		t.Fatalf("exit %d: %s", code, te.err)
	}
}

func TestOtherCommands(t *testing.T) {
	f, srv := newFakeAPI(t)
	id := "0b5a7f1e-0000-4000-8000-000000000001"
	f.handle("GET /emails", `{"object":"list","has_more":true,"data":[{"id":"`+id+`","to":["ann@example.com"],"subject":"Hello","last_event":"delivered","created_at":"2026-10-20T10:00:00Z"}]}`)
	f.handle("GET /emails/"+id, `{"object":"email","id":"`+id+`","from":"hi@acme.com","to":["ann@example.com"],"subject":"Hello","text":"Body here","last_event":"delivered"}`)
	f.handle("GET /emails/"+id+"/events", `{"object":"list","data":[{"type":"email.bounced","recipient":"ann@example.com","payload":{"bounce":{"type":"hard","message":"5.1.1 no such user"}},"created_at":"2026-10-20T10:00:00Z"}]}`)
	f.handle("POST /emails/"+id+"/cancel", `{"object":"email","id":"`+id+`"}`)
	f.handle("POST /domains", domainJSON)
	f.handle("POST /domains/"+"6a1d2c3e-1111-4222-8333-444455556666"+"/verify", `{"object":"domain","id":"6a1d2c3e-1111-4222-8333-444455556666"}`)
	f.handle("GET /inbound/emails", `{"object":"list","data":[{"id":"i1","from":"x@y.com","to":["in@acme.com"],"subject":"Re: hi"}]}`)
	f.handle("GET /inbound/emails/i1", `{"id":"i1","from":"x@y.com","to":["in@acme.com"],"subject":"Re: hi","text":"thanks","attachments":[{"id":"a1","filename":"f.pdf","content_type":"application/pdf","size":12}]}`)
	f.handle("POST /webhooks", `{"object":"webhook","id":"w1","endpoint":"https://x.test/h","events":["email.delivered","email.bounced"],"signing_secret":"whsec_abc"}`)
	f.handle("DELETE /webhooks/w1", `{}`)
	f.handle("GET /api-keys", `{"object":"list","data":[{"id":"k1","name":"ci","permission":"sending_access","token_prefix":"us_abc"}]}`)
	f.handle("POST /api-keys", `{"id":"k2","token":"us_new_key_secret_value_123456"}`)
	f.handle("DELETE /api-keys/k1", `{}`)
	f.handle("GET /logs", `{"object":"list","data":[{"method":"POST","path":"/emails","status":422,"duration_ms":12,"source":"api","error_message":"bad"}]}`)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"emails", "list", "--limit", "1", "--to", "ann@example.com"}, []string{"Hello", "delivered", "--after " + id}},
		{[]string{"emails", "get", id}, []string{"Body here", "hi@acme.com"}},
		{[]string{"emails", "events", id}, []string{"email.bounced", "5.1.1 no such user"}},
		{[]string{"emails", "cancel", id}, []string{"Canceled"}},
		{[]string{"domains", "add", "acme.com"}, []string{"Added acme.com", "usk3m9q2xa._domainkey"}},
		{[]string{"domains", "verify", "6a1d2c3e-1111-4222-8333-444455556666"}, []string{"Verification started"}},
		{[]string{"inbound", "list"}, []string{"Re: hi"}},
		{[]string{"inbound", "get", "i1"}, []string{"thanks", "f.pdf"}},
		{[]string{"webhooks", "create", "--endpoint", "https://x.test/h", "--event", "email.delivered,email.bounced"}, []string{"whsec_abc"}},
		{[]string{"webhooks", "delete", "w1"}, []string{"Deleted webhook w1"}},
		{[]string{"api-keys", "list"}, []string{"sending_access", "us_abc"}},
		{[]string{"api-keys", "create", "--name", "ci"}, []string{"us_new_key_secret_value_123456"}},
		{[]string{"api-keys", "delete", "k1", "--yes"}, []string{"Deleted API key k1"}},
		{[]string{"logs", "list", "--status", "4xx", "--days", "7"}, []string{"/emails", "422"}},
		{[]string{"version"}, []string{"usend " + Version}},
	}
	for _, c := range cases {
		if code := te.run(c.args...); code != 0 {
			t.Errorf("%v: exit %d: %s", c.args, code, te.err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(te.out.String(), w) {
				t.Errorf("%v: missing %q in:\n%s", c.args, w, te.out)
			}
		}
		assertNoKey(t, te)
	}

	// Query parameters reach the API.
	te.run("logs", "list", "--status", "4xx", "--days", "7")
	if q := f.last().Query; !strings.Contains(q, "status=4xx") || !strings.Contains(q, "days=7") {
		t.Errorf("logs query = %q", q)
	}
	// Webhook events are split on commas.
	te.run("webhooks", "create", "--endpoint", "https://x.test/h", "--event", "email.delivered,email.bounced")
	if b := string(f.last().Body); !strings.Contains(b, `"events":["email.delivered","email.bounced"]`) {
		t.Errorf("webhook body = %s", b)
	}
}

func TestAPIKeyCreateSaveProfile(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("POST /api-keys", `{"id":"k2","token":"us_new_key_secret_value_123456"}`)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey
	if code := te.run("api-keys", "create", "--name", "ci", "--permission", "sending", "--save-profile", "ci"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	if strings.Contains(te.out.String(), "us_new_key_secret_value_123456") {
		t.Fatalf("saved key printed: %s", te.out)
	}
	if b := string(f.last().Body); !strings.Contains(b, `"permission":"sending_access"`) {
		t.Errorf("body = %s", b)
	}
	b, _ := os.ReadFile(te.configFile())
	if !strings.Contains(string(b), "us_new_key_secret_value_123456") {
		t.Fatalf("config = %s", b)
	}
}

func TestDoctor(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("GET /domains", `{"object":"list","data":[]}`)
	te := newEnv(t, srv.URL)
	te.vars["USEND_API_KEY"] = testKey
	if code := te.run("doctor"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, te.out, te.err)
	}
	if out := te.out.String(); !strings.Contains(out, "OK    Network") || !strings.Contains(out, "key accepted (full access)") {
		t.Fatalf("doctor: %s", out)
	}
	assertNoKey(t, te)

	te.vars["USEND_API_KEY"] = "us_revoked_key_000000000"
	if code := te.run("doctor", "--json"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	var res struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}
	if err := json.Unmarshal(te.out.Bytes(), &res); err != nil || res.OK {
		t.Fatalf("doctor json: %s", te.out)
	}

	// Unreachable API.
	te.vars["USEND_API_KEY"] = testKey
	te.vars["USEND_BASE_URL"] = "http://127.0.0.1:1"
	if code := te.run("doctor"); code != 1 || !strings.Contains(te.out.String(), "FAIL  Network") {
		t.Fatalf("exit %d: %s", code, te.out)
	}
}

func TestConfigPermissionsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	f, srv := newFakeAPI(t)
	f.handle("GET /domains", `{"object":"list","data":[]}`)
	te := newEnv(t, srv.URL)
	os.MkdirAll(filepath.Dir(te.configFile()), 0o755)
	os.WriteFile(te.configFile(), []byte(`{"profiles":{}}`), 0o644)
	if code := te.run("login", "--key", testKey); code != 0 {
		t.Fatalf("exit %d: %s", code, te.err)
	}
	fi, _ := os.Stat(te.configFile())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}
}

func TestHelpAndUnknown(t *testing.T) {
	te := newEnv(t, "")
	if code := te.run(); code != 0 || !strings.Contains(te.out.String(), "Commands:") {
		t.Fatalf("no args: exit %d %s", code, te.out)
	}
	if code := te.run("domains", "--help"); code != 0 || !strings.Contains(te.out.String(), "verify") {
		t.Fatalf("domains --help: exit %d %s", code, te.out)
	}
	if code := te.run("help", "logs"); code != 0 || !strings.Contains(te.out.String(), "list") {
		t.Fatalf("help logs: exit %d %s", code, te.out)
	}
	if code := te.run("frobnicate"); code != 2 {
		t.Fatalf("unknown: exit %d", code)
	}
	if code := te.run("emails", "frob"); code != 2 {
		t.Fatalf("unknown sub: exit %d", code)
	}
}

func TestMaskKey(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"us_short":                       "us_****",
		"us_live_0123456789abcdefSECRET": "us_liv...CRET",
	} {
		if got := maskKey(in); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", in, got, want)
		}
	}
}
