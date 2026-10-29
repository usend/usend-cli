package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	usend "usend.et/sdk/go"
)

// ---- shared helpers ----

type pageFlags struct {
	limit         int
	after, before string
}

func addPageFlags(fs *flag.FlagSet) *pageFlags {
	p := &pageFlags{}
	fs.IntVar(&p.limit, "limit", 0, "number of rows, 1-100 (default 50)")
	fs.StringVar(&p.after, "after", "", "show rows older than this ID")
	fs.StringVar(&p.before, "before", "", "show rows newer than this ID")
	return p
}

func (p *pageFlags) values() url.Values {
	v := url.Values{}
	if p.limit > 0 {
		v.Set("limit", strconv.Itoa(p.limit))
	}
	if p.after != "" {
		v.Set("after", p.after)
	}
	if p.before != "" {
		v.Set("before", p.before)
	}
	return v
}

func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

func esc(s string) string { return url.PathEscape(s) }

// one requires exactly one positional argument.
func one(pos []string, what, usage string) (string, error) {
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return "", usagef("expected %s (usage: %s)", what, usage)
	}
	return pos[0], nil
}

func none(pos []string, usage string) error {
	if len(pos) > 0 {
		return usagef("unexpected argument %q (usage: %s)", pos[0], usage)
	}
	return nil
}

// show prints raw JSON or calls human.
func (a *app) show(raw json.RawMessage, human func(w io.Writer) error) error {
	if a.wantJSON() {
		return printJSON(a.env.Stdout, raw)
	}
	return human(a.env.Stdout)
}

// showList fetches a list and prints it as JSON or a table.
func (a *app) showList(path, nextCmd string, headers []string, row func(obj) []string, empty string) error {
	raw, err := a.call(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		items, hasMore, err := decodeList(raw)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Fprintln(w, empty)
			return nil
		}
		rows := make([][]string, len(items))
		for i, it := range items {
			rows[i] = row(it)
		}
		table(w, headers, rows)
		more(w, hasMore, items, nextCmd)
		return nil
	})
}

// ---- send ----

func cmdSend(a *app, args []string) error {
	const usage = "usend send --from <address> --to <address> --subject <text> (--html <html> | --html-file <path> | --text <text>) [flags]"
	fs := a.newFlags("send", usage, "Send an email.")
	var to, cc, bcc, replyTo, tags stringList
	from := fs.String("from", "", `sender, e.g. "Acme <hello@acme.com>" (required)`)
	fs.Var(&to, "to", "recipient (repeat for more, max 50)")
	subject := fs.String("subject", "", "subject line")
	html := fs.String("html", "", "HTML body")
	htmlFile := fs.String("html-file", "", `read the HTML body from a file ("-" for stdin)`)
	text := fs.String("text", "", "plain-text body")
	textFile := fs.String("text-file", "", `read the plain-text body from a file ("-" for stdin)`)
	fs.Var(&cc, "cc", "Cc recipient (repeatable)")
	fs.Var(&bcc, "bcc", "Bcc recipient (repeatable)")
	fs.Var(&replyTo, "reply-to", "Reply-To address (repeatable)")
	scheduledAt := fs.String("scheduled-at", "", `send later: RFC 3339 time or a phrase like "in 1 hour" or "tomorrow at 9am"`)
	fs.Var(&tags, "tag", "tag as name=value (repeatable)")
	template := fs.String("template", "", "send a published template (ID or alias) instead of a body")
	var vars stringList
	fs.Var(&vars, "var", "template variable as KEY=value (repeatable)")
	idem := fs.String("idempotency-key", "", "Idempotency-Key header: retries with the same key send once")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	if *from == "" && *template == "" {
		return usagef("--from is required")
	}
	if len(to) == 0 {
		return usagef("--to is required")
	}
	if *html != "" && *htmlFile != "" {
		return usagef("use --html or --html-file, not both")
	}
	if *text != "" && *textFile != "" {
		return usagef("use --text or --text-file, not both")
	}
	if *htmlFile == "-" && *textFile == "-" {
		return usagef("only one of --html-file and --text-file can read stdin")
	}
	if *htmlFile != "" {
		b, err := a.readFile(*htmlFile)
		if err != nil {
			return err
		}
		*html = string(b)
	}
	if *textFile != "" {
		b, err := a.readFile(*textFile)
		if err != nil {
			return err
		}
		*text = string(b)
	}
	if *template == "" && *html == "" && *text == "" {
		return usagef("give a body: --html, --html-file, --text, --text-file or --template")
	}
	if *template != "" && (*html != "" || *text != "") {
		return usagef("--template cannot be combined with --html or --text")
	}
	if *template == "" && len(vars) > 0 {
		return usagef("--var needs --template")
	}
	p := &usend.SendEmailParams{
		From: *from, To: to, Subject: *subject, HTML: *html, Text: *text,
		Cc: cc, Bcc: bcc, ReplyTo: replyTo, ScheduledAt: *scheduledAt,
	}
	for _, t := range tags {
		name, value, ok := strings.Cut(t, "=")
		if !ok || name == "" {
			return usagef("--tag %q: use name=value", t)
		}
		p.Tags = append(p.Tags, usend.Tag{Name: name, Value: value})
	}
	if *template != "" {
		p.Template = &usend.EmailTemplate{ID: *template}
		for _, v := range vars {
			k, val, ok := strings.Cut(v, "=")
			if !ok || k == "" {
				return usagef("--var %q: use KEY=value", v)
			}
			if p.Template.Variables == nil {
				p.Template.Variables = map[string]any{}
			}
			p.Template.Variables[k] = val
		}
	}
	var opts []usend.RequestOption
	if *idem != "" {
		opts = append(opts, usend.WithIdempotencyKey(*idem))
	}
	raw, err := a.call(http.MethodPost, "/emails", p, opts...)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		m, err := decodeObj(raw)
		if err != nil {
			return err
		}
		if *scheduledAt != "" {
			fmt.Fprintf(w, "Scheduled email %s\n", m.s("id"))
		} else {
			fmt.Fprintf(w, "Sent email %s\n", m.s("id"))
		}
		fmt.Fprintf(w, "Follow it with: usend emails get %s\n", m.s("id"))
		return nil
	})
}

func (a *app) readFile(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(a.env.Stdin)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return b, nil
}

// ---- emails ----

func cmdEmails(a *app, args []string) error {
	return a.group("emails", "List, inspect and cancel sent emails.", args, []command{
		{"list", "List sent emails, newest first", emailsList},
		{"get", "Show one email", emailsGet},
		{"events", "Show an email's delivery events", emailsEvents},
		{"cancel", "Cancel a scheduled email", emailsCancel},
	})
}

func emailsList(a *app, args []string) error {
	const usage = "usend emails list [--limit n] [--after id] [--before id] [--to address]"
	fs := a.newFlags("emails list", usage, "List sent emails, newest first.")
	pf := addPageFlags(fs)
	to := fs.String("to", "", "only emails to this recipient")
	tagName := fs.String("tag-name", "", "only emails with this tag name")
	tagValue := fs.String("tag-value", "", "only emails with this tag value")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	q := pf.values()
	for k, v := range map[string]string{"to": *to, "tag_name": *tagName, "tag_value": *tagValue} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return a.showList(withQuery("/emails", q), "usend emails list",
		[]string{"ID", "TO", "SUBJECT", "STATUS", "CREATED"},
		func(e obj) []string {
			return []string{e.s("id"), trunc(e.s("to"), 32), trunc(e.s("subject"), 40), e.s("last_event"), when(e["created_at"])}
		}, "No emails yet.")
}

func emailsGet(a *app, args []string) error {
	const usage = "usend emails get <id>"
	fs := a.newFlags("emails get", usage, "Show one sent email.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "an email ID", usage)
	if err != nil {
		return err
	}
	raw, err := a.call(http.MethodGet, "/emails/"+esc(id), nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		e, err := decodeObj(raw)
		if err != nil {
			return err
		}
		fields(w, [][2]string{
			{"ID", e.s("id")},
			{"From", e.s("from")},
			{"To", e.s("to")},
			{"Cc", e.s("cc")},
			{"Bcc", e.s("bcc")},
			{"Reply-To", e.s("reply_to")},
			{"Subject", e.s("subject")},
			{"Status", e.s("last_event")},
			{"Created", when(e["created_at"])},
			{"Scheduled", when(e["scheduled_at"])},
			{"Tags", e.s("tags")},
			{"Opens", e.s("open_count")},
			{"Clicks", e.s("click_count")},
			{"Broadcast", e.s("broadcast_id")},
		})
		if t, _ := e["text"].(string); t != "" {
			fmt.Fprintf(w, "\n%s\n", strings.TrimRight(t, "\n"))
		} else if _, ok := e["html"].(string); ok {
			fmt.Fprintln(w, "\n(HTML body only: use --json to see it)")
		}
		return nil
	})
}

func emailsEvents(a *app, args []string) error {
	const usage = "usend emails events <id>"
	fs := a.newFlags("emails events", usage, "Show an email's delivery events, oldest first.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "an email ID", usage)
	if err != nil {
		return err
	}
	return a.showList("/emails/"+esc(id)+"/events", "",
		[]string{"TIME", "EVENT", "RECIPIENT", "DETAIL"},
		func(e obj) []string {
			return []string{when(e["created_at"]), e.s("type"), e.s("recipient"), trunc(eventDetail(e.obj("payload")), 60)}
		}, "No events yet.")
}

// eventDetail picks the useful part of an event payload (bounce reason, clicked link).
func eventDetail(p obj) string {
	if p == nil {
		return "-"
	}
	if b := p.obj("bounce"); b != nil {
		return strings.TrimSpace(b.s("type") + " " + b.s("message"))
	}
	if c := p.obj("click"); c != nil {
		return c.s("link")
	}
	if c := p.obj("complaint"); c != nil {
		return c.s("feedback_type")
	}
	if m, ok := p["message"].(string); ok && m != "" {
		return m
	}
	return "-"
}

func emailsCancel(a *app, args []string) error {
	const usage = "usend emails cancel <id>"
	fs := a.newFlags("emails cancel", usage, "Cancel a scheduled email.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "an email ID", usage)
	if err != nil {
		return err
	}
	raw, err := a.call(http.MethodPost, "/emails/"+esc(id)+"/cancel", nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		fmt.Fprintf(w, "Canceled email %s\n", id)
		return nil
	})
}

// ---- domains ----

func cmdDomains(a *app, args []string) error {
	return a.group("domains", "List, add, inspect and verify sending domains.", args, []command{
		{"list", "List domains", domainsList},
		{"get", "Show a domain and its DNS records", domainsGet},
		{"add", "Add a domain", domainsAdd},
		{"verify", "Check a domain's DNS records again", domainsVerify},
	})
}

func domainsList(a *app, args []string) error {
	const usage = "usend domains list"
	fs := a.newFlags("domains list", usage, "List your domains.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	return a.showList("/domains", "",
		[]string{"ID", "NAME", "STATUS", "RECEIVING", "CREATED"},
		func(d obj) []string {
			return []string{d.s("id"), d.s("name"), d.s("status"), d.s("receiving_enabled"), when(d["created_at"])}
		}, "No domains yet. Add one with: usend domains add example.com")
}

func looksLikeID(s string) bool {
	return len(s) == 36 && strings.Count(s, "-") == 4
}

// domainID accepts a domain ID or a domain name.
func (a *app) domainID(arg string) (string, error) {
	if looksLikeID(arg) || !strings.Contains(arg, ".") {
		return arg, nil
	}
	raw, err := a.call(http.MethodGet, "/domains", nil)
	if err != nil {
		return "", err
	}
	items, _, err := decodeList(raw)
	if err != nil {
		return "", err
	}
	for _, d := range items {
		if strings.EqualFold(d.s("name"), arg) {
			return d.s("id"), nil
		}
	}
	return "", fmt.Errorf("no domain named %s in this organization (see usend domains list)", arg)
}

func domainsGet(a *app, args []string) error {
	const usage = "usend domains get <id or name>"
	fs := a.newFlags("domains get", usage, "Show a domain and the DNS records to publish.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	arg, err := one(pos, "a domain ID or name", usage)
	if err != nil {
		return err
	}
	id, err := a.domainID(arg)
	if err != nil {
		return err
	}
	raw, err := a.call(http.MethodGet, "/domains/"+esc(id), nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error { return printDomain(w, raw) })
}

func printDomain(w io.Writer, raw json.RawMessage) error {
	d, err := decodeObj(raw)
	if err != nil {
		return err
	}
	fields(w, [][2]string{
		{"ID", d.s("id")},
		{"Name", d.s("name")},
		{"Status", d.s("status")},
		{"Region", d.s("region")},
		{"DNS host", d.s("dns_provider")},
		{"Receiving", d.s("receiving_enabled")},
		{"Open tracking", d.s("open_tracking")},
		{"Click tracking", d.s("click_tracking")},
		{"Created", when(d["created_at"])},
	})
	records := d.list("records")
	if len(records) > 0 {
		fmt.Fprintf(w, "\nDNS records (names are relative to %s; @ is the domain itself):\n\n", d.s("name"))
		rows := make([][]string, len(records))
		for i, r := range records {
			prio := r.s("priority")
			rows[i] = []string{r.s("record"), r.s("type"), r.s("name"), prio, r.s("status"), r.s("value")}
		}
		table(w, []string{"RECORD", "TYPE", "NAME", "PRIORITY", "STATUS", "VALUE"}, rows)
	}
	if hints := d.list("hints"); len(hints) > 0 {
		fmt.Fprintln(w, "\nHints:")
		for _, h := range hints {
			fmt.Fprintf(w, "  %s: %s\n", h.s("record"), h.s("message"))
		}
	}
	if d.s("status") != "verified" && len(records) > 0 {
		fmt.Fprintf(w, "\nAfter publishing the records, run: usend domains verify %s\n", d.s("id"))
	}
	return nil
}

func domainsAdd(a *app, args []string) error {
	const usage = "usend domains add <name> [--return-path label]"
	fs := a.newFlags("domains add", usage, "Add a sending domain and print the DNS records to publish.")
	returnPath := fs.String("return-path", "", `return-path subdomain label (default "send")`)
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	name, err := one(pos, "a domain name", usage)
	if err != nil {
		return err
	}
	body := map[string]any{"name": name}
	if *returnPath != "" {
		body["custom_return_path"] = *returnPath
	}
	raw, err := a.call(http.MethodPost, "/domains", body)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		fmt.Fprintf(w, "Added %s\n\n", name)
		return printDomain(w, raw)
	})
}

func domainsVerify(a *app, args []string) error {
	const usage = "usend domains verify <id or name>"
	fs := a.newFlags("domains verify", usage, "Start a new check of a domain's DNS records.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	arg, err := one(pos, "a domain ID or name", usage)
	if err != nil {
		return err
	}
	id, err := a.domainID(arg)
	if err != nil {
		return err
	}
	raw, err := a.call(http.MethodPost, "/domains/"+esc(id)+"/verify", nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		fmt.Fprintf(w, "Verification started for %s. It runs in the background; check with: usend domains get %s\n", arg, arg)
		return nil
	})
}

// ---- inbound ----

func cmdInbound(a *app, args []string) error {
	return a.group("inbound", "Read emails received at your domains.", args, []command{
		{"list", "List received emails, newest first", inboundList},
		{"get", "Show one received email", inboundGet},
	})
}

func inboundList(a *app, args []string) error {
	const usage = "usend inbound list [--limit n] [--after id] [--before id] [--to address] [--from text] [--search text]"
	fs := a.newFlags("inbound list", usage, "List received emails, newest first.")
	pf := addPageFlags(fs)
	to := fs.String("to", "", "only mail to this recipient")
	from := fs.String("from", "", "only mail whose sender contains this")
	search := fs.String("search", "", "subject or sender contains this")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	q := pf.values()
	for k, v := range map[string]string{"to": *to, "from": *from, "search": *search} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return a.showList(withQuery("/inbound/emails", q), "usend inbound list",
		[]string{"ID", "FROM", "TO", "SUBJECT", "RECEIVED"},
		func(e obj) []string {
			return []string{e.s("id"), trunc(e.s("from"), 32), trunc(e.s("to"), 32), trunc(e.s("subject"), 40), when(e["created_at"])}
		}, "No received emails yet.")
}

func inboundGet(a *app, args []string) error {
	const usage = "usend inbound get <id>"
	fs := a.newFlags("inbound get", usage, "Show one received email.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "an email ID", usage)
	if err != nil {
		return err
	}
	raw, err := a.call(http.MethodGet, "/inbound/emails/"+esc(id), nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		e, err := decodeObj(raw)
		if err != nil {
			return err
		}
		fields(w, [][2]string{
			{"ID", e.s("id")},
			{"From", e.s("from")},
			{"To", e.s("to")},
			{"Cc", e.s("cc")},
			{"Reply-To", e.s("reply_to")},
			{"Subject", e.s("subject")},
			{"Received", when(e["created_at"])},
		})
		if atts := e.list("attachments"); len(atts) > 0 {
			fmt.Fprintln(w, "\nAttachments:")
			rows := make([][]string, len(atts))
			for i, at := range atts {
				rows[i] = []string{at.s("filename"), at.s("content_type"), at.s("size"), at.s("id")}
			}
			table(w, []string{"FILENAME", "TYPE", "BYTES", "ID"}, rows)
		}
		if t, _ := e["text"].(string); t != "" {
			fmt.Fprintf(w, "\n%s\n", strings.TrimRight(t, "\n"))
		} else if h, _ := e["html"].(string); h != "" {
			fmt.Fprintln(w, "\n(HTML body only: use --json to see it)")
		}
		return nil
	})
}

// ---- webhooks ----

func cmdWebhooks(a *app, args []string) error {
	return a.group("webhooks", "List, create and delete webhook endpoints.", args, []command{
		{"list", "List webhooks", webhooksList},
		{"create", "Create a webhook", webhooksCreate},
		{"delete", "Delete a webhook", webhooksDelete},
	})
}

func webhooksList(a *app, args []string) error {
	const usage = "usend webhooks list"
	fs := a.newFlags("webhooks list", usage, "List webhook endpoints.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	return a.showList("/webhooks", "",
		[]string{"ID", "ENDPOINT", "EVENTS", "ENABLED", "CREATED"},
		func(h obj) []string {
			return []string{h.s("id"), h.s("endpoint"), trunc(h.s("events"), 50), h.s("enabled"), when(h["created_at"])}
		}, "No webhooks yet. Create one with: usend webhooks create --endpoint https://... --event email.delivered")
}

func webhooksCreate(a *app, args []string) error {
	const usage = "usend webhooks create --endpoint <https url> --event <type> [--event <type> ...]"
	fs := a.newFlags("webhooks create", usage, "Create a webhook endpoint. Events: email.sent, email.delivered, email.bounced, email.complained, email.opened, email.clicked, email.failed, email.received and more (see the docs).")
	endpoint := fs.String("endpoint", "", "URL that receives the events (required)")
	var events stringList
	fs.Var(&events, "event", "event type to send (repeatable or comma-separated; required)")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	if *endpoint == "" {
		return usagef("--endpoint is required")
	}
	ev := splitList(events)
	if len(ev) == 0 {
		return usagef("give at least one --event")
	}
	raw, err := a.call(http.MethodPost, "/webhooks", map[string]any{"endpoint": *endpoint, "events": ev})
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		h, err := decodeObj(raw)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Created webhook %s\n\n", h.s("id"))
		fields(w, [][2]string{
			{"Endpoint", h.s("endpoint")},
			{"Events", h.s("events")},
			{"Signing secret", h.s("signing_secret")},
		})
		fmt.Fprintln(w, "\nVerify each delivery's signature with the signing secret; keep it private.")
		return nil
	})
}

func webhooksDelete(a *app, args []string) error {
	const usage = "usend webhooks delete <id> [--yes]"
	fs := a.newFlags("webhooks delete", usage, "Delete a webhook endpoint.")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "a webhook ID", usage)
	if err != nil {
		return err
	}
	if err := a.confirm("Delete webhook "+id+"?", *yes); err != nil {
		return err
	}
	raw, err := a.call(http.MethodDelete, "/webhooks/"+esc(id), nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		fmt.Fprintf(w, "Deleted webhook %s\n", id)
		return nil
	})
}

// ---- api keys ----

func cmdAPIKeys(a *app, args []string) error {
	return a.group("api-keys", "List, create and delete API keys (needs a full-access key).", args, []command{
		{"list", "List API keys", apiKeysList},
		{"create", "Create an API key", apiKeysCreate},
		{"delete", "Delete an API key", apiKeysDelete},
	})
}

func apiKeysList(a *app, args []string) error {
	const usage = "usend api-keys list"
	fs := a.newFlags("api-keys list", usage, "List API keys. The secret part is never shown again after creation.")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	return a.showList("/api-keys", "",
		[]string{"ID", "NAME", "PERMISSION", "PREFIX", "DOMAIN", "LAST USED", "CREATED"},
		func(k obj) []string {
			return []string{k.s("id"), k.s("name"), k.s("permission"), k.s("token_prefix"), k.s("domain_id"), when(k["last_used_at"]), when(k["created_at"])}
		}, "No API keys.")
}

func apiKeysCreate(a *app, args []string) error {
	const usage = "usend api-keys create --name <name> [--permission full_access|sending_access] [--domain-id id] [--save-profile name]"
	fs := a.newFlags("api-keys create", usage, "Create an API key. Its secret is shown once.")
	name := fs.String("name", "", "a name for the key (required)")
	perm := fs.String("permission", "full_access", "full_access or sending_access")
	domainID := fs.String("domain-id", "", "limit a sending_access key to one domain")
	save := fs.String("save-profile", "", "save the new key as this profile instead of printing it")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--name is required")
	}
	switch *perm {
	case "full", "full_access":
		*perm = "full_access"
	case "sending", "sending_access":
		*perm = "sending_access"
	default:
		return usagef("--permission must be full_access or sending_access")
	}
	body := map[string]any{"name": *name, "permission": *perm}
	if *domainID != "" {
		body["domain_id"] = *domainID
	}
	raw, err := a.call(http.MethodPost, "/api-keys", body)
	if err != nil {
		return err
	}
	k, err := decodeObj(raw)
	if err != nil {
		return err
	}
	token, _ := k["token"].(string)
	if *save != "" {
		c, _ := a.credentials()
		cfg, path, err := a.loadConfig()
		if err != nil {
			return err
		}
		p := &Profile{APIKey: token, Permission: *perm}
		if c != nil && c.URLSource != "default" {
			p.BaseURL = c.BaseURL
		}
		cfg.Profiles[*save] = p
		if err := saveConfig(path, cfg); err != nil {
			return err
		}
		if a.wantJSON() {
			return printValue(a.env.Stdout, map[string]any{"id": k.s("id"), "profile": *save, "key": maskKey(token)})
		}
		fmt.Fprintf(a.env.Stdout, "Created API key %s (%s) and saved it as profile %q. Use it with --profile %s.\n", k.s("id"), maskKey(token), *save, *save)
		return nil
	}
	return a.show(raw, func(w io.Writer) error {
		fmt.Fprintf(w, "Created API key %s\n\n%s\n\nCopy it now: it is not shown again.\n", k.s("id"), token)
		return nil
	})
}

func apiKeysDelete(a *app, args []string) error {
	const usage = "usend api-keys delete <id> [--yes]"
	fs := a.newFlags("api-keys delete", usage, "Delete an API key. Programs using it stop working at once.")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "an API key ID", usage)
	if err != nil {
		return err
	}
	if err := a.confirm("Delete API key "+id+"? Programs using it stop working.", *yes); err != nil {
		return err
	}
	raw, err := a.call(http.MethodDelete, "/api-keys/"+esc(id), nil)
	if err != nil {
		return err
	}
	return a.show(raw, func(w io.Writer) error {
		fmt.Fprintf(w, "Deleted API key %s\n", id)
		return nil
	})
}

// ---- logs ----

func cmdLogs(a *app, args []string) error {
	return a.group("logs", "Read the API request log (needs a full-access key).", args, []command{
		{"list", "List logged API requests, newest first", logsList},
	})
}

func logsList(a *app, args []string) error {
	const usage = "usend logs list [--status 4xx] [--method POST] [--source api|dashboard] [--days n] [--search text]"
	fs := a.newFlags("logs list", usage, "List logged API requests, newest first.")
	pf := addPageFlags(fs)
	status := fs.String("status", "", "status code or class: 422, 2xx, 4xx, 5xx")
	method := fs.String("method", "", "HTTP method")
	source := fs.String("source", "", "api or dashboard")
	days := fs.Int("days", 0, "how many days back, 1-30 (default 15)")
	search := fs.String("search", "", "endpoint, path, error or user agent contains this")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if err := none(pos, usage); err != nil {
		return err
	}
	q := pf.values()
	for k, v := range map[string]string{"status": *status, "method": strings.ToUpper(*method), "source": *source, "q": *search} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *days > 0 {
		q.Set("days", strconv.Itoa(*days))
	}
	return a.showList(withQuery("/logs", q), "usend logs list",
		[]string{"TIME", "METHOD", "PATH", "STATUS", "MS", "SOURCE", "ERROR"},
		func(l obj) []string {
			return []string{when(l["created_at"]), l.s("method"), trunc(l.s("path"), 48), l.s("status"), l.s("duration_ms"), l.s("source"), trunc(l.s("error_message"), 40)}
		}, "No requests logged in this period.")
}
