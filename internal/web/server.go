// Package web is coolix's web UI: server-rendered pages, background deploy
// jobs with live logs over server-sent events, and journald log streaming.
package web

import (
	"bufio"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"coolix/internal/appdef"
	"coolix/internal/engine"
	"coolix/internal/webhook"

	"github.com/go-webauthn/webauthn/webauthn"
)

//go:embed templates static
var assets embed.FS

type Server struct {
	Config        *engine.Config
	PasswordFile  string
	SessionKey    []byte
	SecureCookies bool
	Jobs          Jobs

	logins loginLimiter

	// Accounts holds the admin account; defaults to <stateDir>/account.json.
	Accounts *AccountStore
	// WebAuthnOrigins are the origins passkeys work on; empty disables them.
	WebAuthnOrigins []string
	webauthn        *webauthn.WebAuthn
	ceremonies      ceremonies

	hookMu     sync.Mutex
	hookQueued map[string]*Job // webhook deploys waiting for the deploy lock

	pages map[string]*template.Template
}

func (s *Server) engine(j *Job) *engine.Engine { return &engine.Engine{Config: s.Config, Out: j} }

// quiet is an engine for read-only and repo-editing calls from handlers.
func (s *Server) quiet() *engine.Engine { return &engine.Engine{Config: s.Config, Out: log.Writer()} }

var funcs = template.FuncMap{
	"join":     strings.Join,
	"shortRev": engine.ShortRev,
	"bytes": func(n int64) string {
		switch {
		case n >= 1<<30:
			return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
		}
		return fmt.Sprintf("%d B", n)
	},
	"ago": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		}
		return t.Format("2006-01-02")
	},
	"envLines": func(env map[string]string) string {
		var b strings.Builder
		for _, k := range sortedKeys(env) {
			fmt.Fprintf(&b, "%s=%s\n", k, env[k])
		}
		return b.String()
	},
	"hasDB": func(a *appdef.App, kind string) bool {
		for _, d := range a.Databases {
			if d.Kind == kind {
				return true
			}
		}
		return false
	},
	"dbVersion": func(a *appdef.App) string {
		for _, d := range a.Databases {
			if d.Kind == "postgres" {
				return d.Version
			}
		}
		return ""
	},
	"healthPath": func(a *appdef.App) string {
		if a.Health == nil {
			return ""
		}
		return a.Health.Path
	},
	"healthTimeout": func(a *appdef.App) string {
		if a.Health == nil {
			return ""
		}
		return a.Health.Timeout
	},
}

func (s *Server) Handler() (http.Handler, error) {
	if s.Accounts == nil {
		s.Accounts = &AccountStore{Path: filepath.Join(s.Config.StateDir, "account.json")}
	}
	if err := s.initWebAuthn(); err != nil {
		return nil, fmt.Errorf("passkeys: %w", err)
	}
	s.pages = map[string]*template.Template{}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		page := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if page == "layout" || page == "form" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/form.html", n)
		if err != nil {
			return nil, err
		}
		s.pages[page] = t
	}

	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /setup", s.handleSetup)
	mux.HandleFunc("POST /setup", s.handleSetup)
	mux.HandleFunc("POST /login/passkey/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("POST /login/passkey/finish", s.handlePasskeyLoginFinish)
	mux.HandleFunc("POST /hooks/{name}", s.handleWebhook)

	auth := http.NewServeMux()
	auth.HandleFunc("POST /logout", s.handleLogout)
	auth.HandleFunc("GET /{$}", s.handleDashboard)
	auth.HandleFunc("POST /deploy", s.handleDeployAll)
	auth.HandleFunc("GET /apps/new", s.handleNewApp)
	auth.HandleFunc("POST /apps", s.handleCreateApp)
	auth.HandleFunc("GET /apps/{name}", s.handleApp)
	auth.HandleFunc("POST /apps/{name}", s.handleUpdateApp)
	auth.HandleFunc("POST /apps/{name}/deploy", s.handleDeployApp)
	auth.HandleFunc("POST /apps/{name}/delete", s.handleDeleteApp)
	auth.HandleFunc("POST /apps/{name}/secrets", s.handleSetSecret)
	auth.HandleFunc("POST /apps/{name}/secrets/{key}/delete", s.handleUnsetSecret)
	auth.HandleFunc("GET /apps/{name}/logs", s.handleAppLogs)
	auth.HandleFunc("POST /apps/{name}/webhook", s.handleWebhookSettings)
	auth.HandleFunc("POST /apps/{name}/backups", s.handleBackup)
	auth.HandleFunc("POST /apps/{name}/backups/{file}/restore", s.handleRestore)
	auth.HandleFunc("GET /jobs/{id}", s.handleJob)
	auth.HandleFunc("GET /jobs/{id}/events", s.handleJobEvents)
	auth.HandleFunc("GET /generations", s.handleGenerations)
	auth.HandleFunc("GET /account", s.handleAccount)
	auth.HandleFunc("POST /account/password", s.handleSetPassword)
	auth.HandleFunc("POST /account/password/delete", s.handleRemovePassword)
	auth.HandleFunc("POST /account/passkeys/begin", s.handlePasskeyRegisterBegin)
	auth.HandleFunc("POST /account/passkeys/finish", s.handlePasskeyRegisterFinish)
	auth.HandleFunc("POST /account/passkeys/{id}/delete", s.handleRemovePasskey)
	auth.HandleFunc("POST /generations/{n}/rollback", s.handleRollback)
	mux.Handle("/", s.requireAuth(auth))

	return securityHeaders(mux), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "no such page", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Page"] = page
	data["Authed"] = s.validSession(r)
	data["Flash"] = r.URL.Query().Get("msg")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		path += "?msg=" + urlQueryEscape(msg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// Pages ------------------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	st, err := s.quiet().Status()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "dashboard", map[string]any{
		"Title":  "Apps",
		"Status": st,
		"Jobs":   s.Jobs.Recent(8, ""),
	})
}

func (s *Server) handleNewApp(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "new", map[string]any{"Title": "New app", "App": &appdef.App{}})
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	app := &appdef.App{Name: strings.TrimSpace(r.FormValue("name")), PortBase: 1}
	err := applyForm(app, r)
	if err == nil {
		err = app.Validate()
	}
	if err == nil {
		err = s.quiet().CreateApp(app)
	}
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, "new", map[string]any{"Title": "New app", "App": app, "Error": err.Error()})
		return
	}
	redirect(w, r, "/apps/"+app.Name, "App created. Deploy it to start it.")
}

func (s *Server) appData(r *http.Request, name string) (map[string]any, error) {
	e := s.quiet()
	app, err := e.App(name)
	if err != nil {
		return nil, err
	}
	st, err := e.Status()
	if err != nil {
		return nil, err
	}
	var status engine.AppStatus
	for _, a := range st.Apps {
		if a.Name == name {
			status = a
		}
	}
	secrets, err := e.Secrets(name)
	if err != nil {
		return nil, err
	}
	token, err := e.WebhookToken(name)
	if err != nil {
		return nil, err
	}
	data := map[string]any{
		"Title":      name,
		"App":        app,
		"AppState":   status,
		"UpToDate":   st.UpToDate,
		"Secrets":    secrets,
		"Jobs":       s.Jobs.Recent(5, name),
		"Webhook":    token != "",
		"WebhookURL": webhookURL(r, name),
	}
	if app.HasDatabase("postgres") {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		backups, err := e.Backups(ctx, name)
		data["Backups"], data["BackupsErr"] = backups, err
	}
	return data, nil
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	data, err := s.appData(r, r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "app", data)
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	e := s.quiet()
	if _, err := e.App(name); err != nil {
		http.NotFound(w, r)
		return
	}
	app, err := e.ModifyApp(name, func(a *appdef.App) error { return applyForm(a, r) })
	if err != nil {
		data, derr := s.appData(r, name)
		if derr != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		data["App"], data["Error"] = app, err.Error()
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, "app", data)
		return
	}
	redirect(w, r, "/apps/"+name, "Saved. Deploy to apply.")
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if r.FormValue("confirm") != name {
		redirect(w, r, "/apps/"+name, "Type the app name to confirm deletion.")
		return
	}
	if err := s.quiet().RemoveApp(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	j := s.startDeploy("")
	redirect(w, r, j.URL(), "")
}

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	key := strings.TrimSpace(r.FormValue("key"))
	if err := s.quiet().SetSecret(name, key, r.FormValue("value")); err != nil {
		redirect(w, r, "/apps/"+name, "Secret not saved: "+err.Error())
		return
	}
	redirect(w, r, "/apps/"+name, "Secret "+key+" saved. Deploy to apply.")
}

func (s *Server) handleUnsetSecret(w http.ResponseWriter, r *http.Request) {
	name, key := r.PathValue("name"), r.PathValue("key")
	if err := s.quiet().UnsetSecret(name, key); err != nil {
		redirect(w, r, "/apps/"+name, err.Error())
		return
	}
	redirect(w, r, "/apps/"+name, "Secret "+key+" removed. Deploy to apply.")
}

func (s *Server) handleGenerations(w http.ResponseWriter, r *http.Request) {
	gens, err := s.quiet().Generations()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Newest first.
	for i, j := 0, len(gens)-1; i < j; i, j = i+1, j-1 {
		gens[i], gens[j] = gens[j], gens[i]
	}
	s.render(w, r, "generations", map[string]any{"Title": "Generations", "Generations": gens})
}

// Jobs -------------------------------------------------------------------

func (s *Server) startDeploy(app string) *Job {
	title := "Deploy all apps"
	if app != "" {
		title = "Deploy " + app
	}
	return s.Jobs.Start(title, app, func(ctx context.Context, j *Job) error {
		return s.engine(j).Deploy(ctx, app)
	})
}

func (s *Server) handleDeployAll(w http.ResponseWriter, r *http.Request) {
	redirect(w, r, s.startDeploy("").URL(), "")
}

func (s *Server) handleDeployApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.quiet().App(name); err != nil {
		http.NotFound(w, r)
		return
	}
	redirect(w, r, s.startDeploy(name).URL(), "")
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		http.NotFound(w, r)
		return
	}
	j := s.Jobs.Start(fmt.Sprintf("Roll back to generation %d", n), "", func(ctx context.Context, j *Job) error {
		return s.engine(j).Rollback(ctx, n)
	})
	redirect(w, r, j.URL(), "")
}

func (s *Server) job(r *http.Request) *Job {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return nil
	}
	return s.Jobs.Get(id)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	j := s.job(r)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	lines, _, _, _ := j.Snapshot(0)
	s.render(w, r, "job", map[string]any{"Title": j.Title, "Job": j, "Log": strings.Join(lines, "\n")})
}

// sse prepares w for server-sent events.
func sse(w http.ResponseWriter) (func(event, data string), bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return func(event, data string) {
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		for _, line := range strings.Split(data, "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		fl.Flush()
	}, true
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	j := s.job(r)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	send, ok := sse(w)
	if !ok {
		return
	}
	sent := 0
	for {
		lines, done, _, next := j.Snapshot(sent)
		for _, l := range lines {
			send("", l)
		}
		sent += len(lines)
		if done {
			send("done", j.State())
			return
		}
		select {
		case <-next:
		case <-r.Context().Done():
			return
		}
	}
}

// handleAppLogs streams the app's (and its databases') journal over SSE.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.quiet().App(name); err != nil {
		http.NotFound(w, r)
		return
	}
	send, ok := sse(w)
	if !ok {
		return
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.quiet().Logs(r.Context(), name, 200, true, pw))
	}()
	defer pr.Close()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		send("", sc.Text())
	}
	if err := sc.Err(); err != nil && r.Context().Err() == nil {
		send("", "error: "+err.Error())
	}
}

// Webhooks ---------------------------------------------------------------

// handleWebhook deploys an app when its forge reports a push. It's the only
// unauthenticated POST: the request must carry the app's webhook signature.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	e := s.quiet()
	// Unknown apps, apps without a webhook and bad signatures all look the
	// same, so the endpoint doesn't reveal which apps exist.
	token, err := e.WebhookToken(name)
	if err != nil || token == "" {
		http.Error(w, webhook.ErrUnauthorized.Error(), http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	d, err := webhook.Verify(r.Header, body, token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	app, err := e.App(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case d.Ping():
		fmt.Fprintln(w, `{"status":"pong"}`)
		return
	case !d.Push():
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "{\"status\":\"ignored\",\"reason\":%q}\n", "event "+d.Event)
		return
	case !d.Matches(app.WebhookBranch()):
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "{\"status\":\"ignored\",\"reason\":%q}\n", d.Ref+" is not "+app.WebhookBranch())
		return
	}
	// A deploy that hasn't started yet will pick up this push too, so
	// bursts of pushes don't queue one full deploy each.
	s.hookMu.Lock()
	if j := s.hookQueued[name]; j != nil {
		s.hookMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "{\"status\":\"queued\",\"job\":%d}\n", j.ID)
		return
	}
	j := s.Jobs.Start("Deploy "+name+" (webhook from "+d.Provider+")", name, func(ctx context.Context, j *Job) error {
		e := s.engine(j)
		e.OnDeployStart = func() { s.dequeueHook(name, j) }
		defer s.dequeueHook(name, j)
		return e.Deploy(ctx, name)
	})
	if s.hookQueued == nil {
		s.hookQueued = map[string]*Job{}
	}
	s.hookQueued[name] = j
	s.hookMu.Unlock()
	log.Printf("webhook: %s push %s: deploying %s as job %d", d.Provider, d.Ref, name, j.ID)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "{\"status\":\"deploying\",\"job\":%d}\n", j.ID)
}

func (s *Server) handleWebhookSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	e := s.quiet()
	if r.FormValue("action") == "disable" {
		if err := e.DisableWebhook(name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		redirect(w, r, "/apps/"+name, "Webhook disabled.")
		return
	}
	token, err := e.EnableWebhook(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.appData(r, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Shown once, on this response only: it's not put in a URL or a flash.
	data["NewWebhookToken"] = token
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "app", data)
}

// Backups ----------------------------------------------------------------

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.quiet().App(name); err != nil {
		http.NotFound(w, r)
		return
	}
	j := s.Jobs.Start("Back up "+name, name, func(ctx context.Context, j *Job) error {
		return s.engine(j).Backup(ctx, name)
	})
	redirect(w, r, j.URL(), "")
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	name, file := r.PathValue("name"), r.PathValue("file")
	if r.FormValue("confirm") != name {
		redirect(w, r, "/apps/"+name, "Type the app name to confirm the restore.")
		return
	}
	j := s.Jobs.Start("Restore "+name+" from "+file, name, func(ctx context.Context, j *Job) error {
		return s.engine(j).Restore(ctx, name, file)
	})
	redirect(w, r, j.URL(), "")
}

// webhookURL is where the forge should send deliveries, as seen by this
// request (the UI is normally behind Caddy on its own domain).
func webhookURL(r *http.Request, app string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/hooks/" + app
}

// Forms ------------------------------------------------------------------

// applyForm copies the settings form onto app.
func applyForm(app *appdef.App, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	app.Source = strings.TrimSpace(r.FormValue("source"))
	app.Package = strings.TrimSpace(r.FormValue("package"))
	app.Command = strings.TrimSpace(r.FormValue("command"))
	app.PreStart = strings.TrimSpace(r.FormValue("pre_start"))
	app.PreStartTimeout = strings.TrimSpace(r.FormValue("pre_start_timeout"))
	app.Path = strings.Fields(r.FormValue("path"))
	app.Domains = strings.Fields(r.FormValue("domains"))

	path, timeout := strings.TrimSpace(r.FormValue("health_path")), strings.TrimSpace(r.FormValue("health_timeout"))
	app.Health = nil
	if path != "" || timeout != "" {
		app.Health = &appdef.Health{Path: path, Timeout: timeout}
	}

	app.Env = nil
	for i, line := range strings.Split(r.FormValue("env"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("environment line %d: want KEY=VALUE", i+1)
		}
		if app.Env == nil {
			app.Env = map[string]string{}
		}
		app.Env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	app.Resources = nil
	res := appdef.Resources{MemoryMax: strings.TrimSpace(r.FormValue("memory_max")), CPUQuota: strings.TrimSpace(r.FormValue("cpu_quota"))}
	if v := strings.TrimSpace(r.FormValue("tasks_max")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("tasks limit %q: want a number", v)
		}
		res.TasksMax = n
	}
	if res != (appdef.Resources{}) {
		app.Resources = &res
	}

	app.Backup = nil
	backup := appdef.Backup{Schedule: strings.TrimSpace(r.FormValue("backup_schedule"))}
	if v := strings.TrimSpace(r.FormValue("backup_keep")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("backups to keep %q: want a number", v)
		}
		backup.Keep = n
	}
	if backup != (appdef.Backup{}) {
		app.Backup = &backup
	}

	app.Webhook = nil
	if b := strings.TrimSpace(r.FormValue("webhook_branch")); b != "" {
		app.Webhook = &appdef.Webhook{Branch: b}
	}

	app.Databases = nil
	if r.FormValue("db_postgres") != "" {
		app.Databases = append(app.Databases, appdef.Database{Kind: "postgres", Version: strings.TrimSpace(r.FormValue("pg_version"))})
	}
	if r.FormValue("db_redis") != "" {
		app.Databases = append(app.Databases, appdef.Database{Kind: "redis"})
	}
	return nil
}

func (s *Server) dequeueHook(app string, j *Job) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if s.hookQueued[app] == j {
		delete(s.hookQueued, app)
	}
}
