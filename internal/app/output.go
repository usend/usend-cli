package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// obj is a decoded JSON object.
type obj map[string]any

func decodeObj(raw json.RawMessage) (obj, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m obj
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	return m, nil
}

// decodeList reads { data: [...] } (or a bare array) into objects.
func decodeList(raw json.RawMessage) ([]obj, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	var items []any
	hasMore := false
	if bytes.HasPrefix(trimmed, []byte("[")) {
		d := json.NewDecoder(bytes.NewReader(trimmed))
		d.UseNumber()
		if err := d.Decode(&items); err != nil {
			return nil, false, fmt.Errorf("unexpected response: %w", err)
		}
	} else {
		m, err := decodeObj(raw)
		if err != nil {
			return nil, false, err
		}
		items, _ = m["data"].([]any)
		hasMore, _ = m["has_more"].(bool)
	}
	out := make([]obj, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, obj(m))
		}
	}
	return out, hasMore, nil
}

// s formats a field for a table cell.
func (o obj) s(key string) string { return cell(o[key]) }

func (o obj) obj(key string) obj {
	m, _ := o[key].(map[string]any)
	return obj(m)
}

func (o obj) list(key string) []obj {
	items, _ := o[key].([]any)
	var out []obj
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, obj(m))
		}
	}
	return out
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		if x == "" {
			return "-"
		}
		return x
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case json.Number:
		return x.String()
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if m, ok := e.(map[string]any); ok && m["name"] != nil {
				parts = append(parts, fmt.Sprintf("%v=%v", m["name"], m["value"]))
				continue
			}
			parts = append(parts, cell(e))
		}
		if len(parts) == 0 {
			return "-"
		}
		return strings.Join(parts, ", ")
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// when formats an RFC 3339 timestamp in local time.
func when(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// trunc shortens s to n runes for a table cell.
func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

// printJSON pretty-prints a raw API response.
func printJSON(w io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = w.Write(append(raw, '\n'))
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// printValue prints a Go value as indented JSON.
func printValue(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// table prints aligned columns.
func table(w io.Writer, headers []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// fields prints label/value pairs, skipping empty values.
func fields(w io.Writer, pairs [][2]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range pairs {
		if p[1] == "" || p[1] == "-" {
			continue
		}
		fmt.Fprintf(tw, "%s:\t%s\n", p[0], p[1])
	}
	tw.Flush()
}

// more prints a pagination hint under a table.
func more(w io.Writer, hasMore bool, rows []obj, cmd string) {
	if hasMore && len(rows) > 0 {
		fmt.Fprintf(w, "\nMore rows: %s --after %s\n", cmd, rows[len(rows)-1].s("id"))
	}
}
