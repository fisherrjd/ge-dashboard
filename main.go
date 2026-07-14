// ge-dashboard: the human-facing web UI over ge-orchestrator. Stateless —
// all data comes from the orchestrator API; /api/* is reverse-proxied so the
// browser can reach SSE run feeds and trigger runs from the same origin.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/osrs-ge/ge-dashboard/internal/orch"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

type server struct {
	orch *orch.Client
	tpl  *template.Template
	md   goldmark.Markdown
}

func main() {
	log.SetPrefix("ge-dashboard: ")
	orchURL := getenv("GE_DASHBOARD_ORCH_URL", "http://127.0.0.1:8410")
	addr := getenv("GE_DASHBOARD_ADDR", ":8420")

	funcs := template.FuncMap{
		"gp":      formatGp,
		"pct":     func(f *float64) string { if f == nil { return "—" }; return fmt.Sprintf("%.2f%%", *f) },
		"ratio":   func(f *float64) string { if f == nil { return "—" }; return fmt.Sprintf("%.2f", *f) },
		"since":   since,
		"verdictClass": verdictClass,
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))

	s := &server{
		orch: orch.New(orchURL),
		tpl:  tpl,
		md:   goldmark.New(goldmark.WithExtensions(extension.GFM)),
	}

	target, err := url.Parse(orchURL)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // SSE: flush every write

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	mux.Handle("/api/", proxy)
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /runs", s.runs)
	mux.HandleFunc("POST /runs", s.triggerRun)
	mux.HandleFunc("GET /runs/{id}", s.run)
	mux.HandleFunc("GET /strategies/{id}", s.strategy)
	mux.HandleFunc("GET /scoreboard", s.scoreboard)

	log.Printf("listening on %s (orchestrator: %s)", addr, orchURL)
	log.Fatal(http.ListenAndServe(addr, mux))
}

type page struct {
	Title  string
	Active string
	Err    string
	Data   any
}

func (s *server) render(w http.ResponseWriter, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	list, err := s.orch.LatestStrategiesLive(r.Context())
	p := page{Title: "Actionable now", Active: "index", Data: list}
	if err != nil {
		p.Err = "orchestrator unreachable: " + err.Error()
	}
	s.render(w, "index.html", p)
}

func (s *server) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.orch.Runs(r.Context())
	health, _ := s.orch.Health(r.Context())
	data := map[string]any{"Runs": runs, "Health": health}
	p := page{Title: "Runs", Active: "runs", Data: data}
	if err != nil {
		p.Err = "orchestrator unreachable: " + err.Error()
	}
	s.render(w, "runs.html", p)
}

// triggerRun converts the brief form to the orchestrator's JSON body and
// redirects to the run page (which streams live progress).
func (s *server) triggerRun(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var b strings.Builder
	b.WriteString("{")
	fmt.Fprintf(&b, `"capital_gp": %s`, orInt(r.FormValue("capital_gp"), "100000000"))
	fmt.Fprintf(&b, `, "risk": %q`, orStr(r.FormValue("risk"), "medium"))
	fmt.Fprintf(&b, `, "min_confidence": %q`, orStr(r.FormValue("min_confidence"), "medium"))
	if v := r.FormValue("members"); v == "true" || v == "false" {
		fmt.Fprintf(&b, `, "members": %s`, v)
	}
	b.WriteString(`, "archetypes": {`)
	first := true
	for _, a := range []string{"A", "B", "C", "D", "E", "F"} {
		if v := r.FormValue("w_" + a); v != "" {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, `%q: %s`, a, v)
			first = false
		}
	}
	b.WriteString("}")
	if n := strings.TrimSpace(r.FormValue("notes")); n != "" {
		fmt.Fprintf(&b, `, "notes": %q`, n)
	}
	b.WriteString("}")

	resp, err := http.Post(s.orch.BaseURL+"/api/runs", "application/json", strings.NewReader(b.String()))
	if err != nil {
		http.Error(w, "orchestrator unreachable: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	var out struct {
		RunID       int64  `json:"run_id"`
		ActiveRunID int64  `json:"active_run_id"`
		Error       string `json:"error"`
	}
	if err := jsonDecode(resp.Body, &out); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	switch resp.StatusCode {
	case http.StatusCreated:
		http.Redirect(w, r, fmt.Sprintf("/runs/%d", out.RunID), http.StatusSeeOther)
	case http.StatusConflict:
		http.Redirect(w, r, fmt.Sprintf("/runs/%d", out.ActiveRunID), http.StatusSeeOther)
	default:
		http.Error(w, out.Error, resp.StatusCode)
	}
}

func (s *server) run(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, strategies, err := s.orch.Run(r.Context(), id)
	if err != nil {
		s.render(w, "run.html", page{Title: "Run", Active: "runs", Err: err.Error()})
		return
	}
	var reportHTML template.HTML
	if run.Status == "succeeded" {
		md, err := s.orch.Report(r.Context(), id)
		if err == nil && md != "" {
			var buf strings.Builder
			if err := s.md.Convert([]byte(md), &buf); err == nil {
				reportHTML = template.HTML(buf.String())
			}
		}
	}
	s.render(w, "run.html", page{Title: fmt.Sprintf("Run %d", id), Active: "runs", Data: map[string]any{
		"Run": run, "Strategies": strategies, "ReportHTML": reportHTML,
	}})
}

func (s *server) strategy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st, evals, err := s.orch.Strategy(r.Context(), id)
	if err != nil {
		s.render(w, "strategy.html", page{Title: "Strategy", Active: "index", Err: err.Error()})
		return
	}
	s.render(w, "strategy.html", page{Title: st.Title, Active: "index", Data: map[string]any{
		"S": st, "Evals": evals,
	}})
}

func (s *server) scoreboard(w http.ResponseWriter, r *http.Request) {
	rows, err := s.orch.Scoreboard(r.Context())
	p := page{Title: "Scoreboard", Active: "scoreboard", Data: rows}
	if err != nil {
		p.Err = "orchestrator unreachable: " + err.Error()
	}
	s.render(w, "scoreboard.html", p)
}

// --- helpers ---

func formatGp(n *int64) string {
	if n == nil {
		return "—"
	}
	v := *n
	neg := v < 0
	if neg {
		v = -v
	}
	var s string
	switch {
	case v >= 1_000_000_000:
		s = fmt.Sprintf("%.2fB", float64(v)/1e9)
	case v >= 1_000_000:
		s = fmt.Sprintf("%.2fM", float64(v)/1e6)
	case v >= 10_000:
		s = fmt.Sprintf("%.1fk", float64(v)/1e3)
	default:
		s = fmt.Sprintf("%d", v)
	}
	if neg {
		s = "-" + s
	}
	return s
}

func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh ago", d.Hours())
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func verdictClass(v string) string {
	switch v {
	case "healthy":
		return "ok"
	case "degraded":
		return "warn"
	case "kill_signal":
		return "bad"
	}
	return "unknown"
}

func jsonDecode(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orInt(v, def string) string {
	if _, err := strconv.ParseInt(v, 10, 64); err != nil {
		return def
	}
	return v
}
