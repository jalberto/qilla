package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/artifacts"
	"github.com/jalberto/qilla/internal/doctor"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/reconcile"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := ui.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		b, _ := ui.ReadFile("ui/manifest.json")
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(b)
	})
	mux.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, r *http.Request) {
		b, _ := ui.ReadFile("ui/icon.svg")
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/jobs", s.jobs)
	mux.HandleFunc("POST /api/ask", s.ask)
	mux.HandleFunc("GET /api/chat/history", s.chatHistory)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/doctor", s.doctor)
	mux.HandleFunc("GET /api/runs", s.runs)
	mux.HandleFunc("GET /api/memory", s.memory)
	mux.HandleFunc("GET /api/routines", s.routines)
	mux.HandleFunc("POST /api/routines/{name}/run", s.runRoutine)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("POST /api/jobs/{id}/drop", s.dropJob)
	mux.HandleFunc("POST /api/reconcile", s.reconcileNow)
	mux.HandleFunc("GET /api/artifacts", s.artifactList)
	mux.HandleFunc("GET /artifacts/{id}", s.artifactServe)
	mux.HandleFunc("POST /api/artifacts/{id}/rm", s.artifactRm)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return s.activity(mux)
}

// activity resets the idle timer on every request except the SSE stream.
func (s *Server) activity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			s.touch()
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, replay, cancel := s.hub.subscribe()
	defer cancel()
	for _, e := range replay {
		w.Write([]byte("data: "))
		w.Write(e.line())
		w.Write([]byte("\n\n"))
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.closing:
			return
		case e := <-ch:
			w.Write([]byte("data: "))
			w.Write(e.line())
			w.Write([]byte("\n\n"))
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	js, err := s.Q.List(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, js)
}

func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text  string `json:"text"`
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		http.Error(w, "need {\"text\": …}", 400)
		return
	}
	if in.Agent == "" {
		in.Agent = "chief"
	}
	if _, ok := s.Cfg().Agents[in.Agent]; !ok {
		http.Error(w, "unknown agent", 400)
		return
	}
	id, _, err := s.Q.Enqueue(r.Context(), queue.Job{Routine: "ask", Agent: in.Agent, Text: in.Text, Priority: 5, Due: s.Now()})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.hub.publish("job", "ask queued", map[string]any{"id": id})
	s.kick()
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	day := s.Now().Format("2006-01-02")
	pending, _ := s.Q.Pending(r.Context())
	byRoutine, _ := s.L.Summary(r.Context(), day, "routine")
	byAgent, _ := s.L.Summary(r.Context(), day, "agent")
	must := map[string]bool{}
	for name, rt := range s.Cfg().Routines {
		if rt.MustRun {
			must[name], _ = s.L.SucceededToday(r.Context(), day, name)
		}
	}
	// "subagents" is the sum over every configured agent's session: the Today
	// tab reports machine-wide activity, not one conversation's.
	subagents := 0
	for agent := range s.Cfg().Agents {
		if sid := s.W.Session(agent); sid != "" {
			subagents += openSubagents(transcriptPath(s.Cfg().Vault, sid), sid)
		}
	}
	writeJSON(w, map[string]any{
		"day": day, "pending": pending, "must_run": must,
		"running": s.runningJobs(r.Context()), "subagents": subagents,
		"cost_by_routine": byRoutine, "cost_by_agent": byAgent, "budget": s.Cfg().Budget,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, doctor.Full(s.Cfg(), nil, doctor.Default(s.Cfg().Path)))
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.L.Recent(r.Context(), 30)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, rows)
}

func (s *Server) memory(w http.ResponseWriter, r *http.Request) {
	if s.W.Mem == nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	st, _ := s.W.Mem.Stats(r.Context())
	recent, _ := s.W.Mem.B.Recent(r.Context(), "", 20)
	conflicts, _ := s.W.Mem.B.Conflicts(r.Context(), "")
	promo, _ := s.W.Mem.PromotionCandidates(r.Context())
	writeJSON(w, map[string]any{"enabled": true, "stats": st, "recent": recent, "conflicts": conflicts, "promotable": promo})
}

// routines lists configured routines with their state for the UI.
func (s *Server) routines(w http.ResponseWriter, r *http.Request) {
	day := s.Now().Format("2006-01-02")
	type row struct {
		Name      string  `json:"name"`
		Kind      string  `json:"kind"`
		Schedule  string  `json:"schedule"`
		Window    string  `json:"window"`
		MustRun   bool    `json:"must_run"`
		DoneToday bool    `json:"done_today"`
		Pending   bool    `json:"pending"`
		Cost      float64 `json:"cost_today"`
		Runs      int     `json:"runs_today"`
		Failed    int     `json:"failed_today"`
	}
	sum, _ := s.L.Summary(r.Context(), day, "routine")
	byName := map[string]ledger.Line{}
	for _, l := range sum {
		byName[l.Key] = l
	}
	jobs, _ := s.Q.List(r.Context(), 200)
	pending := map[string]bool{}
	for _, j := range jobs {
		if j.State == queue.Queued || j.State == queue.Running {
			pending[j.Routine] = true
		}
	}
	var out []row
	for name, rt := range s.Cfg().Routines {
		done, _ := s.L.SucceededToday(r.Context(), day, name)
		l := byName[name]
		out = append(out, row{name, rt.Kind, rt.Schedule, rt.Window, rt.MustRun, done, pending[name], l.Cost, l.Runs, l.Failed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

func (s *Server) runRoutine(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rt, ok := s.Cfg().Routines[name]
	if !ok {
		http.Error(w, "unknown routine", 404)
		return
	}
	id, dup, err := s.Q.Enqueue(r.Context(), queue.Job{Routine: name, Agent: rt.Agent, Priority: 3, Due: s.Now()})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.hub.publish("job", fmt.Sprintf("%s queued from the page", name), map[string]any{"id": id, "dup": dup})
	s.kick()
	writeJSON(w, map[string]any{"id": id, "already_queued": dup})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if err := s.Q.Requeue(r.Context(), id, "retried from the page", s.Now()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.hub.publish("job", fmt.Sprintf("#%d retried", id), nil)
	s.kick()
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) dropJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if err := s.Q.Suspend(r.Context(), id, "dropped from the page"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.hub.publish("job", fmt.Sprintf("#%d dropped", id), nil)
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) reconcileNow(w http.ResponseWriter, r *http.Request) {
	out, enq, err := reconcile.RunWith(r.Context(), s.Cfg(), s.Q, s.L, s.Now(), doctor.NextFire())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if enq {
		s.kick()
	}
	s.hub.publish("info", "reconcile ran from the page", out)
	writeJSON(w, out)
}

func (s *Server) artStore() *artifacts.Store {
	st, _ := artifacts.New(s.Cfg().ArtifactsDir(), time.Duration(s.Cfg().Artifacts.TTLDays)*24*time.Hour, s.Cfg().Artifacts.MaxMB)
	return st
}

func (s *Server) artifactList(w http.ResponseWriter, r *http.Request) {
	l, err := s.artStore().List()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if l == nil {
		l = []artifacts.Meta{}
	}
	writeJSON(w, l)
}

func (s *Server) artifactServe(w http.ResponseWriter, r *http.Request) {
	p := s.artStore().Path(r.PathValue("id"))
	if p == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", artifacts.CSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeFile(w, r, p)
}

func (s *Server) artifactRm(w http.ResponseWriter, r *http.Request) {
	if err := s.artStore().Remove(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
