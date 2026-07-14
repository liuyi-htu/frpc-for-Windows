package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed frpc-web.ico
var appIcon []byte

const (
	appName            = "frpc-web"
	serviceFrpc        = "frpc"
	serviceWeb         = "frpc-web"
	defaultWebListenIP = "127.0.0.1"
	defaultWebPort     = 62930
)

func main() {
	cmd := "open"
	argsStart := 1
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = strings.ToLower(os.Args[1])
		argsStart = 2
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	base := fs.String("base", defaultBaseDir(), "install/base directory")
	noDownload := fs.Bool("no-download", false, "do not download frpc.exe during install")
	noDesktopShortcut := fs.Bool("no-desktop-shortcut", false, "do not create desktop shortcut during install")
	version := fs.String("version", "latest", "frp version, for example latest or v0.61.2")
	_ = fs.String("listen", "", "deprecated; listen address is read from frpc.json")
	_ = fs.Parse(repairBaseArgs(os.Args[argsStart:]))

	p := newPaths(*base)
	if err := readWebListenConfig(&p); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	var err error

	switch cmd {
	case "install":
		err = install(p, *noDownload)
	case "install-ui":
		err = installFromUI(p, *noDownload)
	case "install-services":
		err = installServices(p, !*noDesktopShortcut)
	case "install-services-ui":
		err = installServicesNoStart(p, !*noDesktopShortcut)
	case "uninstall":
		err = uninstallServices(p)
	case "open":
		err = openWeb(p)
	case "admin-web":
		err = runAdminWeb(p)
	case "recover-web":
		err = recoverWeb(p)
	case "web":
		err = runWebConsole(p)
	case "frpc":
		err = runFrpcConsole(p)
	case "run-web":
		err = runAsService(p, "web")
	case "run-frpc":
		err = runAsService(p, "frpc")
	case "start":
		err = runCLIServiceAction(p, "start")
	case "stop":
		err = runCLIServiceAction(p, "stop")
	case "restart":
		err = runCLIServiceAction(p, "restart")
	case "status":
		err = printStatus(p)
	case "status-json":
		err = printOpenWrtStatusJSON(p)
	case "action":
		err = runOpenWrtAction(p)
	case "update":
		_, err = updateFrpc(p, *version)
	case "edit":
		err = openFile(p.Config)
	case "help", "-h", "--help":
		printHelp()
	default:
		printHelp()
		err = fmt.Errorf("unknown command: %s", cmd)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println(`frpc-web

Commands:
  install [--base DIR] [--no-download]
                                      install files and frpc Windows service
  uninstall [--base DIR]             uninstall frpc Windows service
  open [--base DIR]
                                      start/open local Web backend
  admin-web [--base DIR]                 run elevated Web backend after old backend exits
  web [--base DIR]                       run Web backend in console mode
  frpc [--base DIR]                      run frpc in console mode
  start | stop | restart | status        control frpc service
  update [--base DIR] [--version latest] download/update frpc.exe
  edit [--base DIR]                      open frpc.toml with default editor

Default base:
  D:\Program Files\frpc-web

State config:
  frpc.json

Examples:
  frpc-web.exe
  frpc-web.exe install
  frpc-web.exe open --base "D:\Program Files\frpc-web"`)
}

func repairBaseArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--base" || a == "-base" {
			out = append(out, a)
			if i+1 >= len(args) {
				continue
			}
			parts := []string{args[i+1]}
			i++
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				parts = append(parts, args[i+1])
				i++
			}
			out = append(out, strings.Join(parts, " "))
			continue
		}
		if strings.HasPrefix(a, "--base=") || strings.HasPrefix(a, "-base=") {
			prefix, value, _ := strings.Cut(a, "=")
			parts := []string{value}
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				parts = append(parts, args[i+1])
				i++
			}
			out = append(out, prefix+"="+strings.Join(parts, " "))
			continue
		}
		out = append(out, a)
	}
	return out
}

func printStatus(p paths) error {
	fmt.Println(statusText(p))
	return nil
}

func statusText(p paths) string {
	var b strings.Builder
	b.WriteString("InstallDir : " + p.Base + "\n")
	b.WriteString("Config     : " + p.Config + "\n")
	b.WriteString("WebListen  : " + webListenAddr(p) + "\n")
	b.WriteString("frpc.exe   : " + existsText(p.FrpcExe) + "\n")
	b.WriteString("ConfigReady: " + strconv.FormatBool(configReady(p)) + "\n")
	b.WriteString("Admin     : " + yesNo(isAdmin()) + "\n")
	b.WriteString("Disabled   : " + switchText(p.Disabled) + "\n")
	b.WriteString("Service " + serviceFrpc + " : " + queryServiceStatus(serviceFrpc) + "\n")
	if isPortOpen(webListenAddr(p)) {
		b.WriteString("Web backend : RUNNING\n")
	} else {
		b.WriteString("Web backend : STOPPED\n")
	}
	return b.String()
}

func controlOpenWrtService(name, action string) error {
	if name != serviceFrpc {
		return fmt.Errorf("unsupported service on Linux: %s", name)
	}
	if action == "restart" {
		if err := controlOpenWrtService(name, "stop"); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		return controlOpenWrtService(name, "start")
	}
	if action != "start" && action != "stop" {
		return fmt.Errorf("unknown service action: %s", action)
	}
	initScript := "/etc/init.d/frpc"
	if _, err := os.Stat(initScript); err == nil {
		out, err := exec.Command(initScript, action).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w\n%s", initScript, action, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if action == "stop" {
		_ = exec.Command("killall", "frpc").Run()
		return nil
	}
	return exec.Command("/usr/bin/frpc", "-c", "/etc/frp/frpc.toml").Start()
}

func openWrtFrpcRunning() bool {
	if runtime.GOOS == "windows" {
		return false
	}
	checks := [][]string{
		{"pidof", "frpc"},
		{"pgrep", "-f", "(/usr/bin/)?frpc( |$)"},
	}
	for _, args := range checks {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			return true
		}
	}
	return false
}

func printOpenWrtStatusJSON(p paths) error {
	webMode := "minimal"
	if isPortOpen(webListenAddr(p)) {
		webMode = "resident"
	}
	frpcState := "stopped"
	if openWrtFrpcRunning() {
		frpcState = "running"
	}
	data, err := json.Marshal(map[string]string{
		"webMode":   webMode,
		"frpcState": frpcState,
	})
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runOpenWrtAction(p paths) error {
	if isPortOpen(webListenAddr(p)) {
		if openWrtFrpcRunning() {
			return runFrpcServiceAction(p, "stop")
		}
		return runFrpcServiceAction(p, "start")
	}
	return startOpenWrtWebResident(p)
}

func startOpenWrtWebResident(p paths) error {
	if err := ensureDirs(p); err != nil {
		return err
	}
	_ = writeSwitch(p.WebOff, false)
	self := p.SelfExe
	if self == "" {
		var err error
		self, err = os.Executable()
		if err != nil {
			return err
		}
	}
	cmd := exec.Command(self, "web", "--base", p.Base)
	cmd.Dir = "/"
	if runtime.GOOS != "windows" {
		devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
		if err == nil {
			defer devNull.Close()
			cmd.Stdin = devNull
			cmd.Stdout = devNull
			cmd.Stderr = devNull
		}
	}
	return cmd.Start()
}

func yesNo(v bool) string {
	if v {
		return "YES"
	}
	return "NO"
}

func webBackendHealthy(p paths) bool {
	client := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get(webHealthURL(p))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return strings.Contains(string(body), `"app":"frpc-web-go"`)
}

func localAddrHasPort(addr string, port int) bool {
	_, p, err := net.SplitHostPort(addr)
	if err == nil {
		return p == strconv.Itoa(port)
	}
	return strings.HasSuffix(addr, ":"+strconv.Itoa(port))
}

func isPortOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 600*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ---------------- service runner ----------------

type svcProgram struct {
	paths paths
	mode  string
	mu    sync.Mutex
	cmd   *exec.Cmd
	http  *http.Server
	stop  <-chan struct{}
}

func (s *svcProgram) stopProgram() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.http.Shutdown(ctx)
		cancel()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (s *svcProgram) run() {
	_ = ensureDirs(s.paths)
	switch s.mode {
	case "frpc", "run-frpc":
		s.runFrpcLoop()
	case "web", "run-web":
		s.runWebServer()
	}
}

func runFrpcConsole(p paths) error {
	_ = ensureDirs(p)
	sp := &svcProgram{paths: p, mode: "frpc", stop: make(chan struct{})}
	sp.runFrpcLoop()
	return nil
}

func (s *svcProgram) runFrpcLoop() {
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		if switchOn(s.paths.Disabled) {
			logLine(s.paths, "service.log", "stopped: frpc disabled")
			if !sleepOrStop(s.stop, 5*time.Second) {
				return
			}
			continue
		}
		if !configReady(s.paths) {
			logLine(s.paths, "service.log", "stopped: config not ready")
			if !sleepOrStop(s.stop, 5*time.Second) {
				return
			}
			continue
		}
		cmd := exec.Command(s.paths.FrpcExe, "-c", s.paths.Config)
		cmd.Dir = s.paths.Base
		_, frpcLogPath := logPath(s.paths, "frpc")
		frpcLog, err := os.OpenFile(frpcLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			cmd.Stdout = frpcLog
			cmd.Stderr = frpcLog
		}
		s.mu.Lock()
		s.cmd = cmd
		s.mu.Unlock()
		err = cmd.Start()
		if err != nil {
			logLine(s.paths, "service.log", "stopped: failed to start frpc.exe: "+err.Error())
			if frpcLog != nil {
				_ = frpcLog.Close()
			}
			if !sleepOrStop(s.stop, 5*time.Second) {
				return
			}
			continue
		}
		logLine(s.paths, "service.log", fmt.Sprintf("started: frpc.exe pid=%d", cmd.Process.Pid))
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-s.stop:
			_ = cmd.Process.Kill()
			<-done
			if frpcLog != nil {
				_ = frpcLog.Close()
			}
			logLine(s.paths, "service.log", "stopped: frpc.exe stopped by service")
			return
		case err := <-done:
			if frpcLog != nil {
				_ = frpcLog.Close()
			}
			logLine(s.paths, "service.log", "stopped: frpc.exe exited: "+fmt.Sprint(err))
			if !sleepOrStop(s.stop, 3*time.Second) {
				return
			}
		}
	}
}

func sleepOrStop(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

// ---------------- frpc updater ----------------

// ---------------- web backend ----------------

func runWebConsole(p paths) error {
	_ = ensureDirs(p)
	sp := &svcProgram{paths: p, mode: "web", stop: make(chan struct{})}
	sp.runWebServer()
	return nil
}

func (s *svcProgram) runWebServer() {
	if switchOn(s.paths.WebOff) {
		logLine(s.paths, "web.log", "stopped: web disabled")
		return
	}
	mux := http.NewServeMux()
	web := &webApp{paths: s.paths, stop: s.stop}
	web.routes(mux)
	addr := webListenAddr(s.paths)
	url := webURL(s.paths)
	srv := &http.Server{Addr: addr, Handler: mux}
	s.mu.Lock()
	s.http = srv
	s.mu.Unlock()
	if s.stop != nil {
		go func() {
			<-s.stop
			s.stopProgram()
		}()
	}
	_ = url
	logLine(s.paths, "web.log", "started: "+webListenURL(s.paths))
	if err := srv.ListenAndServe(); errors.Is(err, http.ErrServerClosed) {
		logLine(s.paths, "web.log", "stopped: web server stopped")
	} else if err != nil {
		logLine(s.paths, "web.log", "stopped: listen failed: "+err.Error())
	}
}

type webApp struct {
	paths paths
	stop  <-chan struct{}
}

func (w *webApp) routes(mux *http.ServeMux) {
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/favicon.ico", w.favicon)
	mux.HandleFunc("/api/health", w.apiHealth)
	mux.HandleFunc("/api/auth-state", w.apiAuthState)
	mux.HandleFunc("/api/setup", w.apiSetup)
	mux.HandleFunc("/api/login", w.apiLogin)
	mux.HandleFunc("/api/logout", w.apiLogout)
	mux.HandleFunc("/api/account", w.requireAuth(w.apiAccount))
	mux.HandleFunc("/api/status", w.requireAuth(w.apiStatus))
	mux.HandleFunc("/api/start", w.requireAuth(w.apiStart))
	mux.HandleFunc("/api/stop", w.requireAuth(w.apiStop))
	mux.HandleFunc("/api/restart", w.requireAuth(w.apiRestart))
	mux.HandleFunc("/api/low-memory", w.requireAuth(w.apiLowMemory))
	mux.HandleFunc("/api/admin-restart", w.requireAuth(w.apiAdminRestart))
	mux.HandleFunc("/api/update", w.requireAuth(w.apiUpdate))
	mux.HandleFunc("/api/update-upload", w.requireAuth(w.apiUpdateUpload))
	mux.HandleFunc("/api/install", w.requireAuth(w.apiInstall))
	mux.HandleFunc("/api/uninstall", w.requireAuth(w.apiUninstall))
	mux.HandleFunc("/api/basic", w.requireAuth(w.apiBasic))
	mux.HandleFunc("/api/basic-save", w.requireAuth(w.apiBasicSave))
	mux.HandleFunc("/api/config", w.requireAuth(w.apiConfig))
	mux.HandleFunc("/api/save", w.requireAuth(w.apiSave))
	mux.HandleFunc("/api/save-restart", w.requireAuth(w.apiSaveRestart))
	mux.HandleFunc("/api/proxies", w.requireAuth(w.apiProxies))
	mux.HandleFunc("/api/proxy-add", w.requireAuth(w.apiProxyAdd))
	mux.HandleFunc("/api/proxy-delete", w.requireAuth(w.apiProxyDelete))
	mux.HandleFunc("/api/logs", w.requireAuth(w.apiLogs))
	mux.HandleFunc("/api/log-clear", w.requireAuth(w.apiLogClear))
	mux.HandleFunc("/close", w.apiClose)
	mux.HandleFunc("/ping", func(rw http.ResponseWriter, r *http.Request) { writeJSON(rw, map[string]any{"ok": true}) })
}

func (w *webApp) apiHealth(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, map[string]any{"ok": true, "app": "frpc-web-go"})
}

func (w *webApp) apiAuthState(rw http.ResponseWriter, r *http.Request) {
	configured := authConfigured(w.paths)
	username := ""
	if cfg, err := readAuthConfig(w.paths); err == nil {
		username = cfg.Username
	}
	writeJSON(rw, map[string]any{
		"ok":         true,
		"configured": configured,
		"loggedIn":   configured && w.authenticated(r),
		"username":   username,
		"listenIP":   w.paths.ListenIP,
		"listenPort": w.paths.ListenPort,
	})
}

func (w *webApp) apiSetup(rw http.ResponseWriter, r *http.Request) {
	if authConfigured(w.paths) {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": "\u8d26\u53f7\u5df2\u7ecf\u8bbe\u7f6e\u3002"})
		return
	}
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": "\u8bf7\u8f93\u5165\u7528\u6237\u540d\u548c\u5bc6\u7801\u3002"})
		return
	}
	cfg, err := newAuthConfig(req.Username, req.Password)
	if err != nil {
		writeJSONCode(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := writeAuthConfig(w.paths, cfg); err != nil {
		writeJSONCode(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	setSessionCookie(rw, cfg)
	writeJSON(rw, map[string]any{"ok": true, "username": cfg.Username})
}

func (w *webApp) apiLogin(rw http.ResponseWriter, r *http.Request) {
	cfg, err := readAuthConfig(w.paths)
	if err != nil {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": "\u8d26\u53f7\u5c1a\u672a\u8bbe\u7f6e\u3002"})
		return
	}
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Username), cfg.Username) || !verifyPassword(cfg, req.Password) {
		writeJSONCode(rw, http.StatusUnauthorized, map[string]any{"ok": false, "message": "\u7528\u6237\u540d\u6216\u5bc6\u7801\u9519\u8bef\u3002"})
		return
	}
	setSessionCookie(rw, cfg)
	writeJSON(rw, map[string]any{
		"ok":         true,
		"username":   cfg.Username,
		"listenIP":   w.paths.ListenIP,
		"listenPort": w.paths.ListenPort,
	})
}

func (w *webApp) apiLogout(rw http.ResponseWriter, r *http.Request) {
	clearSessionCookie(rw)
	writeJSON(rw, map[string]any{"ok": true})
}

func (w *webApp) apiAccount(rw http.ResponseWriter, r *http.Request) {
	cfg, err := readAuthConfig(w.paths)
	if err != nil {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": "\u8d26\u53f7\u5c1a\u672a\u8bbe\u7f6e\u3002"})
		return
	}
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if currentUsername := strings.TrimSpace(req.CurrentUsername); currentUsername != "" && !strings.EqualFold(currentUsername, cfg.Username) {
		writeJSONCode(rw, http.StatusUnauthorized, map[string]any{"ok": false, "message": "\u5f53\u524d\u7528\u6237\u540d\u9519\u8bef\u3002"})
		return
	}
	if !verifyPassword(cfg, req.CurrentPassword) {
		writeJSONCode(rw, http.StatusUnauthorized, map[string]any{"ok": false, "message": "\u5f53\u524d\u5bc6\u7801\u9519\u8bef\u3002"})
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = cfg.Username
	}
	password := req.NewPassword
	usernameChanged := !strings.EqualFold(username, cfg.Username)
	passwordChanged := password != ""
	listen, listenChanged, err := requestedListenPaths(w.paths, req.ListenIP, req.ListenPort)
	if err != nil {
		writeJSONCode(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if password == "" {
		password = req.CurrentPassword
	}
	next := cfg
	relogin := false
	if usernameChanged || passwordChanged {
		next, err = newAuthConfig(username, password)
		if err != nil {
			writeJSONCode(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		if err := writeAuthConfig(w.paths, next); err != nil {
			writeJSONCode(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		clearSessionCookie(rw)
		relogin = true
	}
	if listenRequested(req.ListenIP, req.ListenPort) {
		if err := writeWebListenConfig(listen); err != nil {
			writeJSONCode(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	restartURL := ""
	if listenChanged {
		if err := restartWebWithListen(listen); err != nil {
			writeJSONCode(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		restartURL = webURL(listen)
	}
	writeJSON(rw, map[string]any{
		"ok":            true,
		"username":      next.Username,
		"relogin":       relogin,
		"listenIP":      listen.ListenIP,
		"listenPort":    listen.ListenPort,
		"listenChanged": listenChanged,
		"restartURL":    restartURL,
	})
	if listenChanged {
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
	}
}

func (w *webApp) favicon(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "image/x-icon")
	_, _ = rw.Write(appIcon)
}



func (w *webApp) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(rw, r, "/ui", http.StatusFound)
		return
	}
	if r.URL.Path != "/ui" && r.URL.Path != "/index.html" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = rw.Write([]byte(indexHTML))
}

func (w *webApp) apiStatus(rw http.ResponseWriter, r *http.Request) {
	out := statusText(w.paths)
	st := "not_running"
	className := "bad"
	if strings.Contains(out, "Service frpc : RUNNING") {
		st, className = "running", "ok"
	} else if strings.Contains(out, "Disabled   : YES") {
		st, className = "stopped", "bad"
	}
	writeJSON(rw, map[string]any{"ok": true, "status": st, "className": className, "output": out})
}

func (w *webApp) apiStart(rw http.ResponseWriter, r *http.Request) {
	w.runServiceAction(rw, "start", "\u542f\u52a8\u547d\u4ee4\u5df2\u53d1\u9001\u3002")
}

func (w *webApp) apiStop(rw http.ResponseWriter, r *http.Request) {
	w.runServiceAction(rw, "stop", "\u505c\u6b62\u547d\u4ee4\u5df2\u53d1\u9001\u3002")
}

func (w *webApp) apiRestart(rw http.ResponseWriter, r *http.Request) {
	w.runServiceAction(rw, "restart", "\u91cd\u542f\u547d\u4ee4\u5df2\u53d1\u9001\u3002")
}

func runFrpcServiceAction(p paths, action string) error {
	switch action {
	case "start":
		_ = writeSwitch(p.Disabled, false)
	case "stop":
		if err := writeSwitch(p.Disabled, true); err != nil {
			return err
		}
	case "restart":
		_ = writeSwitch(p.Disabled, false)
	default:
		return fmt.Errorf("unknown service action: %s", action)
	}
	return controlService(serviceFrpc, action)
}

func runCLIServiceAction(p paths, action string) error {
	_ = ensureDirs(p)
	err := runFrpcServiceAction(p, action)
	if err != nil {
		return err
	}
	return nil
}

func (w *webApp) apiBasic(rw http.ResponseWriter, r *http.Request) {
	text := readText(w.paths.Config)
	writeJSON(rw, map[string]any{
		"ok":         true,
		"serverAddr": tomlValue(text, "serverAddr"),
		"serverPort": tomlValue(text, "serverPort"),
		"user":       tomlValue(text, "user"),
		"dnsServer":  tomlValue(text, "dnsServer"),
		"token":      tomlValue(text, "auth.token"),
		"logLevel":   tomlValue(text, "log.level"),
		"logMaxDays": tomlValue(text, "log.maxDays"),
	})
}

func (w *webApp) apiBasicSave(rw http.ResponseWriter, r *http.Request) {
	var b basicConfig
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSONCode(rw, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := saveBasicConfig(w.paths, b); err != nil {
		writeJSONCode(rw, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "Basic settings saved."})
}

func (w *webApp) apiConfig(rw http.ResponseWriter, r *http.Request) {
	content := readText(w.paths.Config)
	normalized := normalizeConfigText(content)
	if normalized != content {
		_ = os.WriteFile(w.paths.Config, []byte(normalized), 0644)
	}
	writeJSON(rw, map[string]any{"ok": true, "path": w.paths.Config, "content": normalized})
}

func normalizeConfigText(text string) string {
	const fixedProxyComment = "# \u793a\u4f8b\u4ee3\u7406\uff1a\u5c06\u672c\u673a 127.0.0.1:22 \u901a\u8fc7 tcp \u6620\u5c04\u5230\u670d\u52a1\u5668 remotePort 6000"
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(line, "remotePort 6000") && (strings.Contains(line, "\u7f02") || strings.Contains(line, "\u95c2")) {
			if strings.HasSuffix(line, "\r") {
				lines[i] = fixedProxyComment + "\r"
			} else {
				lines[i] = fixedProxyComment
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (w *webApp) apiSave(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := os.WriteFile(w.paths.Config, []byte(body.Content), 0644); err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "Config saved."})
}

func (w *webApp) apiSaveRestart(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := os.WriteFile(w.paths.Config, []byte(body.Content), 0644); err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	_ = writeSwitch(w.paths.Disabled, false)
	out := fmt.Sprint(controlService(serviceFrpc, "restart"))
	writeJSON(rw, map[string]any{"ok": true, "message": "Config saved and frpc restarted.", "output": out})
}

func (w *webApp) apiProxies(rw http.ResponseWriter, r *http.Request) {
	items := listProxies(readText(w.paths.Config))
	writeJSON(rw, map[string]any{"ok": true, "proxies": items})
}

func (w *webApp) apiProxyAdd(rw http.ResponseWriter, r *http.Request) {
	var req proxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONCode(rw, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := addProxy(w.paths, req); err != nil {
		writeJSONCode(rw, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "Proxy added."})
}

func (w *webApp) apiProxyDelete(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Index int `json:"index"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := deleteProxy(w.paths, req.Index); err != nil {
		writeJSONCode(rw, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "Proxy deleted."})
}

func (w *webApp) apiLogs(rw http.ResponseWriter, r *http.Request) {
	which := r.URL.Query().Get("which")
	name, path := logPath(w.paths, which)
	writeJSON(rw, map[string]any{"ok": true, "name": name, "path": path, "content": tailText(path, 220)})
}

func (w *webApp) apiLogClear(rw http.ResponseWriter, r *http.Request) {
	which := r.URL.Query().Get("which")
	name, path := logPath(w.paths, which)
	_ = os.WriteFile(path, nil, 0644)
	writeJSON(rw, map[string]any{"ok": true, "name": name, "path": path, "content": ""})
}

func (w *webApp) apiClose(rw http.ResponseWriter, r *http.Request) {
	_ = writeSwitch(w.paths.WebOff, true)
	writeJSON(rw, map[string]any{"ok": true, "message": "web server closing"})
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.Exit(0)
	}()
}

func writeJSON(rw http.ResponseWriter, v any) { writeJSONCode(rw, 200, v) }

func writeJSONCode(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

// ---------------- config helpers ----------------

type basicConfig struct {
	ServerAddr string `json:"serverAddr"`
	ServerPort string `json:"serverPort"`
	User       string `json:"user"`
	DNSServer  string `json:"dnsServer"`
	Token      string `json:"token"`
	LogLevel   string `json:"logLevel"`
	LogMaxDays string `json:"logMaxDays"`
}

type proxyRequest struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	LocalIP      string `json:"localIP"`
	LocalPort    string `json:"localPort"`
	RemotePort   string `json:"remotePort"`
	CustomDomain string `json:"customDomain"`
}

type proxyItem struct {
	Index         int    `json:"index"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	LocalIP       string `json:"localIP"`
	LocalPort     string `json:"localPort"`
	RemotePort    string `json:"remotePort"`
	CustomDomains string `json:"customDomains"`
}

func readText(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func tomlValue(text, key string) string {
	pattern := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(.+?)\s*$`)
	m := pattern.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	v := strings.TrimSpace(m[1])
	if idx := strings.Index(v, "#"); idx >= 0 {
		v = strings.TrimSpace(v[:idx])
	}
	if strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"") && len(v) >= 2 {
		v = strings.TrimSuffix(strings.TrimPrefix(v, "\""), "\"")
	}
	return strings.ReplaceAll(v, `\"`, `"`)
}

func splitProxyParts(text string) (prefix string, blocks []string) {
	re := regexp.MustCompile(`(?m)^\s*\[\[proxies\]\]\s*$`)
	locs := re.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return strings.TrimRight(text, "\r\n"), nil
	}
	prefix = strings.TrimRight(text[:locs[0][0]], "\r\n")
	for i, loc := range locs {
		start := loc[0]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		blocks = append(blocks, strings.TrimSpace(text[start:end]))
	}
	return prefix, blocks
}

func saveBasicConfig(p paths, b basicConfig) error {
	b.ServerAddr = strings.TrimSpace(b.ServerAddr)
	b.ServerPort = strings.TrimSpace(b.ServerPort)
	b.User = strings.TrimSpace(b.User)
	b.DNSServer = strings.TrimSpace(b.DNSServer)
	b.Token = strings.TrimSpace(b.Token)
	b.LogLevel = strings.ToLower(strings.TrimSpace(b.LogLevel))
	b.LogMaxDays = strings.TrimSpace(b.LogMaxDays)
	if b.LogLevel == "" {
		b.LogLevel = "info"
	}
	if b.LogMaxDays == "" {
		b.LogMaxDays = "7"
	}
	if b.ServerAddr == "" {
		return errors.New("serverAddr cannot be empty")
	}
	if b.ServerPort != "" && !validPort(b.ServerPort) {
		return errors.New("serverPort must be 1-65535")
	}
	if !map[string]bool{"trace": true, "debug": true, "info": true, "warn": true, "error": true}[b.LogLevel] {
		return errors.New("log.level must be trace/debug/info/warn/error")
	}
	if _, err := strconv.Atoi(b.LogMaxDays); err != nil {
		return errors.New("log.maxDays must be a number")
	}
	_, blocks := splitProxyParts(readText(p.Config))
	var out []string
	out = append(out, `serverAddr = "`+tomlEscape(b.ServerAddr)+`"`)
	if b.ServerPort != "" {
		out = append(out, `serverPort = `+b.ServerPort)
	}
	if b.User != "" {
		out = append(out, `user = "`+tomlEscape(b.User)+`"`)
	}
	if b.DNSServer != "" {
		out = append(out, `dnsServer = "`+tomlEscape(b.DNSServer)+`"`)
	}
	out = append(out, "", `log.to = "./logs/frpc.log"`, `log.level = "`+tomlEscape(b.LogLevel)+`"`, `log.maxDays = `+b.LogMaxDays, "")
	if b.Token != "" {
		out = append(out, `auth.method = "token"`, `auth.token = "`+tomlEscape(b.Token)+`"`, "")
	}
	for _, block := range blocks {
		out = append(out, strings.TrimSpace(block), "")
	}
	return os.WriteFile(p.Config, []byte(strings.Join(out, "\r\n")), 0644)
}

func validPort(s string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil && n >= 1 && n <= 65535
}

func tomlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func listProxies(text string) []proxyItem {
	_, blocks := splitProxyParts(text)
	items := make([]proxyItem, 0, len(blocks))
	for i, b := range blocks {
		domains := ""
		dm := regexp.MustCompile(`(?m)^\s*customDomains\s*=\s*\[(.*?)\]\s*$`).FindStringSubmatch(b)
		if len(dm) >= 2 {
			domains = strings.ReplaceAll(dm[1], `"`, "")
			domains = strings.TrimSpace(domains)
		}
		items = append(items, proxyItem{
			Index:         i + 1,
			Name:          tomlValue(b, "name"),
			Type:          tomlValue(b, "type"),
			LocalIP:       tomlValue(b, "localIP"),
			LocalPort:     tomlValue(b, "localPort"),
			RemotePort:    tomlValue(b, "remotePort"),
			CustomDomains: domains,
		})
	}
	return items
}

func addProxy(p paths, req proxyRequest) error {
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.Name = strings.TrimSpace(req.Name)
	req.LocalIP = strings.TrimSpace(req.LocalIP)
	req.LocalPort = strings.TrimSpace(req.LocalPort)
	req.RemotePort = strings.TrimSpace(req.RemotePort)
	req.CustomDomain = strings.TrimSpace(req.CustomDomain)
	if req.Type == "" {
		req.Type = "tcpudp"
	}
	if req.LocalIP == "" {
		req.LocalIP = "127.0.0.1"
	}
	if req.Name == "" {
		return errors.New("Proxy name cannot be empty")
	}
	if !validPort(req.LocalPort) {
		return errors.New("localPort must be 1-65535")
	}
	if req.Type == "http" || req.Type == "https" {
		if req.CustomDomain == "" {
			return errors.New("customDomains is required for http/https")
		}
	} else if !validPort(req.RemotePort) {
		return errors.New("remotePort must be 1-65535")
	}

	text := strings.TrimRight(readText(p.Config), "\r\n")
	var blocks []string
	add := func(name, typ string) {
		var b strings.Builder
		b.WriteString("[[proxies]]\r\n")
		b.WriteString(`name = "` + tomlEscape(name) + "\"\r\n")
		b.WriteString(`type = "` + typ + "\"\r\n")
		b.WriteString(`localIP = "` + tomlEscape(req.LocalIP) + "\"\r\n")
		b.WriteString(`localPort = ` + req.LocalPort + "\r\n")
		if typ == "http" || typ == "https" {
			b.WriteString(`customDomains = ["` + tomlEscape(req.CustomDomain) + "\"]\r\n")
		} else {
			b.WriteString(`remotePort = ` + req.RemotePort + "\r\n")
		}
		blocks = append(blocks, strings.TrimSpace(b.String()))
	}
	switch req.Type {
	case "tcpudp":
		add(req.Name+"-tcp", "tcp")
		add(req.Name+"-udp", "udp")
	case "tcp", "udp", "http", "https":
		add(req.Name+"-"+req.Type, req.Type)
	default:
		return errors.New("proxy type must be tcpudp/tcp/udp/http/https")
	}
	return os.WriteFile(p.Config, []byte(text+"\r\n\r\n"+strings.Join(blocks, "\r\n\r\n")+"\r\n"), 0644)
}

func deleteProxy(p paths, index int) error {
	if index <= 0 {
		return errors.New("invalid index")
	}
	text := readText(p.Config)
	prefix, blocks := splitProxyParts(text)
	if index > len(blocks) {
		return errors.New("proxy index out of range")
	}
	blocks = append(blocks[:index-1], blocks[index:]...)
	var buf bytes.Buffer
	buf.WriteString(strings.TrimRight(prefix, "\r\n"))
	for _, b := range blocks {
		if strings.TrimSpace(b) == "" {
			continue
		}
		buf.WriteString("\r\n\r\n")
		buf.WriteString(strings.TrimSpace(b))
	}
	buf.WriteString("\r\n")
	return os.WriteFile(p.Config, buf.Bytes(), 0644)
}

func tailText(path string, maxLines int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

type authConfig struct {
	Username      string `json:"username"`
	Salt          string `json:"salt"`
	PasswordHash  string `json:"passwordHash"`
	SessionSecret string `json:"sessionSecret"`
}

type authRequest struct {
	CurrentUsername string `json:"currentUsername"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	ListenIP        string `json:"listenIP"`
	ListenPort      string `json:"listenPort"`
}

func authConfigured(p paths) bool {
	cfg, err := readAuthConfig(p)
	return err == nil && cfg.Username != "" && cfg.PasswordHash != "" && cfg.Salt != "" && cfg.SessionSecret != ""
}

func readAuthConfig(p paths) (authConfig, error) {
	var cfg authConfig
	st, err := readFrpcState(p)
	if err != nil {
		return cfg, err
	}
	cfg = st.Auth
	if cfg.Username == "" || cfg.PasswordHash == "" || cfg.Salt == "" || cfg.SessionSecret == "" {
		return cfg, os.ErrNotExist
	}
	return cfg, nil
}

func writeAuthConfig(p paths, cfg authConfig) error {
	st, err := readFrpcState(p)
	if err != nil {
		return err
	}
	st.Auth = cfg
	return writeFrpcState(p, st)
}

func newAuthConfig(username, password string) (authConfig, error) {
	salt, err := randomHex(16)
	if err != nil {
		return authConfig{}, err
	}
	secret, err := randomHex(32)
	if err != nil {
		return authConfig{}, err
	}
	return authConfig{
		Username:      strings.TrimSpace(username),
		Salt:          salt,
		PasswordHash:  passwordHash(salt, password),
		SessionSecret: secret,
	}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func passwordHash(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

func verifyPassword(cfg authConfig, password string) bool {
	got := passwordHash(cfg.Salt, password)
	return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.PasswordHash)) == 1
}

func makeSessionToken(cfg authConfig) string {
	sum := sha256.Sum256([]byte(cfg.SessionSecret + "\x00" + cfg.Username))
	return hex.EncodeToString(sum[:])
}

func setSessionCookie(rw http.ResponseWriter, cfg authConfig) {
	http.SetCookie(rw, &http.Cookie{
		Name:     "frpc_web_session",
		Value:    makeSessionToken(cfg),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   24 * 60 * 60,
	})
}

func clearSessionCookie(rw http.ResponseWriter) {
	http.SetCookie(rw, &http.Cookie{
		Name:     "frpc_web_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (w *webApp) authenticated(r *http.Request) bool {
	cfg, err := readAuthConfig(w.paths)
	if err != nil {
		return false
	}
	c, err := r.Cookie("frpc_web_session")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(makeSessionToken(cfg))) == 1
}

func (w *webApp) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !authConfigured(w.paths) {
			writeJSONCode(rw, http.StatusUnauthorized, map[string]any{"ok": false, "setupRequired": true, "message": "\u8bf7\u5148\u8bbe\u7f6e\u8d26\u53f7\u548c\u5bc6\u7801\u3002"})
			return
		}
		if !w.authenticated(r) {
			writeJSONCode(rw, http.StatusUnauthorized, map[string]any{"ok": false, "loginRequired": true, "message": "\u8bf7\u5148\u767b\u5f55\u3002"})
			return
		}
		h(rw, r)
	}
}

// ---------------- web page ----------------

const oldIndexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>frpc-web for Windows</title>
<style>
:root{color-scheme:light dark;--bg:#f5f7fb;--card:#fff;--text:#172033;--muted:#657083;--line:#dce3ee;--primary:#2563eb;--ok:#16a34a;--bad:#dc2626;--warn:#ca8a04;--shadow:0 8px 26px rgba(15,23,42,.06)}
@media (prefers-color-scheme:dark){:root{--bg:#111827;--card:#1f2937;--text:#f3f4f6;--muted:#aeb6c2;--line:#374151;--primary:#60a5fa;--shadow:none}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font-family:"Microsoft YaHei UI","Segoe UI",system-ui,-apple-system,BlinkMacSystemFont,sans-serif;line-height:1.45}.wrap{max-width:960px;margin:0 auto;padding:16px}.wrap.auth-mode{max-width:none;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:0 18px 18vh;background:#f5f6fb}.card{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:16px;margin:14px 0;box-shadow:var(--shadow)}h1{font-size:24px;margin:4px 0 2px}h3{margin:0 0 10px}.sub{color:var(--muted);font-size:13px}.header-status{font-size:20px;font-weight:850;line-height:1.25;margin:4px 0 6px}.status-ok{color:var(--ok)}.status-bad{color:var(--bad)}.status-warn{color:var(--warn)}.btns{display:flex;flex-wrap:wrap;gap:10px;margin-top:8px}.btn{appearance:none;border:0;border-radius:12px;padding:11px 14px;text-decoration:none;background:var(--primary);color:white;font-weight:700;display:inline-block;cursor:pointer;font-size:14px}.btn.secondary{background:#64748b}.btn.ok{background:var(--ok)}.btn.bad{background:var(--bad)}.btn.warn{background:var(--warn)}.btn:disabled{opacity:.55;cursor:not-allowed}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}.kv{padding:10px;border:1px solid var(--line);border-radius:12px}.k{color:var(--muted);font-size:12px}.v{font-weight:650;word-break:break-all}.mode-tag{display:inline-block;border:1px solid var(--line);border-radius:999px;padding:3px 9px;font-size:12px;color:var(--muted);margin-bottom:8px}input{width:100%;border:1px solid var(--line);border-radius:10px;padding:10px;background:var(--card);color:var(--text);font-size:14px}textarea{width:100%;min-height:62vh;border:1px solid var(--line);border-radius:12px;padding:12px;background:var(--card);color:var(--text);font-family:Consolas,"Cascadia Mono","Courier New",monospace;font-size:13px;line-height:1.55;outline:none}textarea:focus{border-color:var(--primary);box-shadow:0 0 0 3px rgba(37,99,235,.18)}pre{white-space:pre-wrap;word-break:break-word;background:rgba(100,116,139,.12);border:1px solid var(--line);border-radius:12px;padding:12px;max-height:68vh;overflow:auto}.msg{border-left:4px solid var(--primary);padding:10px 12px;background:rgba(37,99,235,.12);border-radius:10px}.small{font-size:12px;color:var(--muted)}.hidden{display:none}.toolbar{display:flex;justify-content:space-between;gap:10px;align-items:center;flex-wrap:wrap}.logtabs .btn{padding:8px 11px;font-size:13px}.form-row{margin:10px 0}label{display:block;font-weight:700;margin:0 0 6px}select{width:100%;border:1px solid var(--line);border-radius:10px;padding:10px;background:var(--card);color:var(--text);font-size:14px}.proxy-item{border:1px solid var(--line);border-radius:12px;padding:10px;margin:10px 0}.proxy-title{font-weight:800}.proxy-meta{color:var(--muted);font-size:12px;word-break:break-all}.auth-card{width:min(720px,100%);display:grid;grid-template-columns:220px minmax(0,1fr);column-gap:30px;align-items:center;background:#fff;border:1px solid #e2e8f0;border-radius:18px;padding:32px 30px 30px;box-shadow:0 18px 48px rgba(15,23,42,.08);margin:0}.auth-card h1{font-size:26px;line-height:1.2;margin:0;font-weight:800;color:#111827;letter-spacing:0}.auth-form{display:grid;gap:16px}.auth-field label{display:block;font-size:17px;line-height:1.25;font-weight:750;color:#111827;margin:0 0 9px}.auth-field input{height:41px;border-radius:10px;border:1px solid #dbe3f2;background:#eaf0ff;color:#111827;font-size:16px;padding:0 16px;box-shadow:inset 0 1px 2px rgba(15,23,42,.04)}.auth-field input:focus{outline:none;border-color:#b8c8f7;box-shadow:0 0 0 3px rgba(59,102,225,.13)}.auth-submit{width:100%;height:37px;border-radius:10px;background:#3d63e6;font-size:15px;margin-top:4px;padding:0 14px}.auth-log{grid-column:2;margin:14px 0 0;min-height:0;max-height:120px;background:transparent;border:0;padding:0;color:#dc2626;font-size:13px}.auth-log:empty{display:none}
@media (max-width:640px){.auth-card{display:block;width:min(420px,100%)}.auth-card h1{margin:0 0 24px}.auth-log{grid-column:auto}}
</style>
</head>
<body><div class="wrap" id="appWrap">
<div class="card head" id="appHeader">
  <h1>frpc-web 闂傚倷鑳堕、濠囨儗閸ヮ剙绀冮柕濞у啫绠戦梻?/h1>
  <div class="header-status status-warn" id="runStatus">frpc 濠电姷顣藉Σ鍛村磻閳ь剟鏌涚€ｎ偅灏扮紒缁樼洴瀵爼骞嬮鐐插闂?..</div>
</div>

<div id="homeView">
  <div class="card">
    <div class="btns">
      <button class="btn ok" onclick="ctl('start')">闂傚倷绀侀幉锟犲礄瑜版帒鍨傞柣妤€鐗婇崣?/button>
      <button class="btn bad" onclick="ctl('stop')">闂傚倷鑳堕…鍫ヮ敄閸℃瑥鍨濈€广儱顦介弫?/button>
      <button class="btn warn" onclick="lowMemory()">闂傚倷绀侀幖顐︽偋閸愵喖纾婚柟鍓х帛閸婂爼鐓崶銊﹀暗缂佺姳鍗抽弻娑㈡倷瀹割喖娈堕梺閫炲苯澧紒瀣浮閳ワ箓宕奸姀鈥冲簥閻庡箍鍎卞ú锕傚磻?/button>
    </div>
  </div>
  <div class="card">
    <h3>闂傚倸鍊烽悞锕€顭垮Ο鑲╃煋闁割偅娲橀崑顏堟煕閳╁喚鐒介柍缁樻楠炴牗娼忛崜褏蓱濠碘槅鍨伴敃顏堝蓟?/h3>
    <div class="btns">
      <button class="btn secondary" onclick="showBasic()">闂傚倷鑳剁涵鍫曞疾閻愭祴鏋嶉柨婵嗩槶閳ь兛绶氬畷銊╊敍濞戞﹩鈧盯姊洪崨濠冨瘷闁告侗鍠氶埀?/button>
      <button class="btn secondary" onclick="showProxies()">婵犵數鍋涢顓熷垔鐎电绶ら柛褎顨呯粻鏍р攽閻樻彃顏☉鎾崇Ч閺屻劌鈽夊Ο渚痪闂?/button>
      <button class="btn secondary" onclick="showConfig()">闂傚倸鍊烽悞锕€顭垮Ο鑲╃煋闁割偅娲橀崑顏堟煕閳╁啰鈽夋俊顐ｏ耿閺屾盯濡烽鐐搭€嶅?/button>
      <button class="btn secondary" onclick="showLogs('service')">闂傚倷绀侀幖顐﹀疮閵娾晛纾块弶鍫氭櫆瀹?/button>
      <button class="btn secondary" onclick="showUpdate()">闂傚倷绀侀幖顐⒚洪妶澶嬪仱闁靛ň鏅涢拑?frpc</button>
      <button class="btn secondary" onclick="showAccount()">闂備浇宕垫慨鐢稿礉瑜忕划濠氬箣閿濆啩姹楅柣鐘充航閸斿秵鍒婃總鍛婄厱闁圭偓顨呴幊蹇涙倵?/button>
      <button class="btn bad" onclick="logout()">闂傚倸鍊风欢锟犲磻閳ь剟鏌涚€ｎ偅宕岄柡灞剧洴楠炲鏁愰崱鈺€鍝楁繝鐢靛仜閹冲矂宕曢幘顔嘉?/button>
    </div>
  </div>
</div>

<div id="authView" class="hidden">
  <div class="auth-card">
    <h1>frpc web 闂傚倷鑳堕、濠囨儗閸ヮ剙绀冮柕濞у啫绠戦梻?/h1>
    <div class="auth-form">
      <div class="auth-field"><label for="authUser">闂傚倷鐒﹀鍨焽閸ф绀夌€广儱顦弰銉︾箾閹存瑥鐏╅悷?/label><input id="authUser" autocomplete="username"></div>
      <div class="auth-field"><label for="authPass">闂備浇顕ч柊锝咁焽瑜嶈灋婵炴垯鍨洪崑?/label><input id="authPass" type="password" autocomplete="current-password"></div>
      <button class="btn auth-submit" onclick="submitAuth()" id="authButton">闂傚倷娴囬惃顐﹀幢閳轰焦顔勭紓?/button>
    </div>
    <pre id="authLog" class="auth-log"></pre>
  </div>
</div>

<div id="accountView" class="hidden">
  <div class="card">
    <div class="toolbar"><div><h3>闂備浇宕垫慨鐢稿礉瑜忕划濠氬箣閿濆啩姹楅柣鐘充航閸斿秵鍒婃總鍛婄厱闁圭偓顨呴幊蹇涙倵?/h3><div class="sub">婵犵數鍎戠徊钘壝归崒鐐茬獥婵°倕鎳庨弸浣糕攽閸屾碍鍟為柛搴＄焸閹綊宕堕鍕缂備礁顦锟犲蓟閿濆缍栨い鎺嗗亾濠碘€茬矙閺屾盯鍩￠崒婊勫垱閻庤娲忛崕閬嶎敇婵傜骞㈡俊銈呭暕缁綁姊虹拠鏌ュ弰婵炰匠鍏炬稑鈽夐姀鈥充函濠电姴锕ら悧鍡氱箽?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <div class="grid">
      <div class="kv"><div class="k">闂傚倷鐒﹀鍨焽閸ф绀夌€广儱顦弰銉︾箾閹存瑥鐏╅悷?/div><input id="accountUser" autocomplete="username"></div>
      <div class="kv"><div class="k">闂佽崵鍠愮划搴㈡櫠濡ゅ懎绠伴柛娑橈攻濞呯娀鏌ｅΟ纰辨毌闁稿鎹囧Λ鍐ㄢ槈濞嗘ɑ锟ラ梻?/div><input id="accountCurrent" type="password" autocomplete="current-password"></div>
      <div class="kv"><div class="k">闂傚倷绀侀幖顐﹀磹閻熼偊鐔嗘慨妞诲亾闁诡垯绶氶獮瀣晜閽樺澹?/div><input id="accountNew" type="password" autocomplete="new-password" placeholder="婵犵數鍋為崹鍫曞箰閸濄儳鐭撻柟鎯版缁犵偤鏌曟繛鐐珔缂備讲鏅犻弻锝夋偄閸涘﹦鍑￠梺璇茬箳閸犳牠寮婚敐澶嬫櫜闁糕剝鐟﹂崳顕€姊?></div>
      <div class="kv"><div class="k">缂傚倷鑳堕搹搴ㄥ矗鎼淬劌绐楁繛鎴欏焺閺佸洤鈹戦悩瀹犲婵☆偅锕㈤弻鈩冨緞鎼淬垻銆婂┑鈥冲级濞茬喖寮?/div><input id="accountConfirm" type="password" autocomplete="new-password"></div>
    </div>
    <div class="btns"><button class="btn ok" onclick="saveAccount()">婵犵數鍎戠徊钘壝洪敂鐐床闁稿瞼鍋為崑銈夋煏婵炑冨椤忔悂姊洪幐搴㈢５闁稿鎸鹃幉鎼佸箯鐏炲墽銆婇梺鐟板槻椤戝鐣峰鍡╂Ь闂?/button></div>
    <pre id="accountLog">Ready.</pre>
  </div>
</div>

<div id="basicView" class="hidden">
  <div class="card">
    <div class="toolbar"><div><h3>闂傚倷鑳剁涵鍫曞疾閻愭祴鏋嶉柨婵嗩槶閳ь兛绶氬畷銊╊敍濞戞﹩鈧盯姊洪崨濠冨瘷闁告侗鍠氶埀?/h3><div class="sub">婵犵數鍎戠徊钘壝洪敂鐐床闁稿瞼鍋為崑銈夋煏婵炵偓娅呴悷娆欑畵楠炴牜鈧稒顭囩粻妯肩磼閹邦収娈旈懣鎰版煕閵夛絽濡跨紒鐘筹耿閺岋繝宕辫箛鎾插闂傚倷鑳剁划顖滃垝鎼淬垺娅犳俊銈傚亾闁挎洏鍨洪幏鍛村捶椤撗勭カ闂備焦瀵х换鍌毭洪敃鍌氱９闁绘劗鍎ら悡?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <div class="grid">
      <div class="kv"><div class="k">serverAddr</div><input id="basicServerAddr" placeholder="example.com or IP"></div>
      <div class="kv"><div class="k">serverPort</div><input id="basicServerPort" inputmode="numeric" placeholder=""></div>
      <div class="kv"><div class="k">user</div><input id="basicUser" placeholder="optional"></div>
      <div class="kv"><div class="k">dnsServer</div><input id="basicDns" placeholder="223.5.5.5"></div>
      <div class="kv"><div class="k">token</div><input id="basicToken" placeholder="optional"></div>
      <div class="kv"><div class="k">log.level</div><input id="basicLogLevel" placeholder="info"></div>
      <div class="kv"><div class="k">log.maxDays</div><input id="basicLogDays" inputmode="numeric" placeholder="7"></div>
    </div>
    <div class="btns"><button class="btn ok" onclick="saveBasic()">婵犵數鍎戠徊钘壝洪敂鐐床闁稿瞼鍋為崑銈夋煏婵炵偓娅呴柡鍕╁劦閺屟嗙疀濮樼厧娈愰梺琛″亾闁靛ň鏅滈崑锝吤归敐鍥剁劸闁抽攱妫冮弻?/button></div>
    <pre id="basicLog">Ready.</pre>
  </div>
</div>

<div id="proxiesView" class="hidden">
  <div class="card">
    <div class="toolbar"><div><h3>婵犵數鍋涢顓熷垔鐎电绶ら柛褎顨呯粻鏍р攽閻樻彃顏☉鎾崇Ч閺屻劌鈽夊Ο渚痪闂?/h3><div class="sub">闂傚倷绀侀幉锛勬暜閻愬绠鹃柍褜鍓氱换娑㈠川椤撶儐鍔夌紓浣稿€圭敮锟犲极閹版澘鐐婇柕濞垮劗閸嬫捇鎮欏ù瀣潔闂佽鍎崇壕顓炵毈缂傚倷娴囨ご鍛婂垔娴犲桅闁圭増婢樼涵鈧梺缁樺姇閻°劑宕曢妷鈺傗拺缂佸娉曠粻鍙夌節閳ь剟宕烽鐐茬亰濡炪倖甯掔€氼剛绮堥崘顔界厱婵炴垵宕楣冩煕閻旈攱鍠橀柡?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <div id="proxyList"><div class="small">闂佽崵鍠愮划搴㈡櫠濡ゅ懎绠伴柛娑橈攻濞呯娀鏌ｅΟ鑽ゃ偞闁哄矉绠撻弻宥夊煛娴ｅ憡娈茬紓浣哄У鐢帡婀侀梺缁樼懃閹虫劙骞冩總鍛婄厵妞ゆ梻銆嬮煬顒€鈹?/div></div>
  </div>
  <div class="card">
    <div class="toolbar"><div><h3>濠电姷鏁搁崕鎴犵礊閳ь剚銇勯弴鍡楀閸欏繘鏌ｉ幇顒夊殶妞も晝鍏橀弻銊モ攽閸℃ê顦╅梺?/h3><div class="sub">闂傚倷娴囬妴鈧柛瀣尰閵囧嫰寮介妸褉妲堥梺?tcp闂傚倷绶氬褍螞閺冨牊鍤曢柛銏㈢┗闂傚倷绶氬褍螞閺冣偓濞煎繐銆掗崓?udp闂傚倷绶氬褍螞閺傛鍤曢柟鍏兼p闂傚倷绶氬褍螞閺傛鍤曢柟鍏兼ps闂?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <div class="grid">
      <div class="kv"><div class="k">婵犵數鍋涢顓熷垔鐎电绶ら柛褎顨呯粻鏍р攽閻樻彃顏悘蹇曟暬閹綊宕堕鍕闂?/div><select id="proxyType" onchange="syncProxyFields()"><option value="tcpudp">tcp + udp</option><option value="tcp">tcp</option><option value="udp">udp</option><option value="http">http</option><option value="https">https</option></select></div>
      <div class="kv"><div class="k">婵犵數鍋涢顓熷垔鐎电绶ら柛褎顨呯粻鏍р攽閻樺弶鎼愰悷娆欏閳ь剙绠嶉崕鍗炩枖?/div><input id="proxyName" placeholder="ssh"></div>
      <div class="kv"><div class="k">闂傚倷绀侀幖顐︽偋濠婂牆绀堟繛鍡楅獜閼拌法鐥悧鍫熸噯</div><input id="proxyLocalIP" value="127.0.0.1"></div>
      <div class="kv"><div class="k">闂傚倷绀侀幖顐︽偋濠婂牆绀堟繛鍡楅獜閼板潡鎮楅棃娑欐喐闁告纰嶉妵鍕冀閵娿劌顥濈紓?/div><input id="proxyLocalPort" inputmode="numeric" placeholder="22"></div>
      <div class="kv" id="remotePortBox"><div class="k">闂備礁鎼ˇ顐﹀疾濠婂懎鍨濋幖娣妼閻撴繈鏌ゅù瀣珖闁告纰嶉妵鍕冀閵娿劌顥濈紓?/div><input id="proxyRemotePort" inputmode="numeric" placeholder="6000"></div>
      <div class="kv hidden" id="domainBox"><div class="k">缂傚倸鍊搁崐鐑芥倿閿曞倸鍨傞柣銏犳啞閸嬧晛螖閿濆懎鏆欓柡鍕╁劦閺岋綁寮崼鐔告殸闂?/div><input id="proxyDomain" placeholder="example.com"></div>
    </div>
    <div class="btns"><button class="btn ok" onclick="addProxy()">濠电姷鏁搁崕鎴犵礊閳ь剚銇勯弴鍡楀閸欏繘鏌ｉ幇顒夊殶妞も晝鍏橀弻銊モ攽閸℃ê顦╅梺?/button></div>
    <pre id="proxyLog">Ready.</pre>
  </div>
</div>

<div id="configView" class="hidden">
  <div class="card">
    <div class="toolbar"><div><h3>闂傚倸鍊烽悞锕€顭垮Ο鑲╃煋闁割偅娲橀崑顏堟煕閳╁啰鈽夋俊顐ｏ耿閺屾盯濡烽鐐搭€嶅?frpc.toml</h3><div class="sub">婵犵數鍎戠徊钘壝洪敂鐐床闁稿瞼鍋為崑銈夋煏婵炵偓娅呴悷娆欑畵楠炴牜鈧稒顭囩粻姗€鏌ｉ悢鍝ョ畵闂囧鏌涜箛娑欙紵濞存嚎鍨介弻娑㈠Χ閸℃瑧鐓夐悗瑙勬礃缁诲牊淇婇幖浣哥厸濞达綀顕栨禒褔姊?frpc闂傚倷鐒︾€笛呯矙閹达附鍤愭い鏍亼閳ь剙鎳撻ˇ瑙勪繆椤愶紕绐旈柛鈹惧亾濡炪倖甯掔€氼參宕戦妸鈺傜厽闁哄倹瀵ч崯鐐寸箾閸涱厾绠婚柟顔肩秺瀹曞爼顢楁担瑙勬濠电姭鎷冮崟顓犵厜閻庤娲樼换鍫熶繆閹间礁唯闁靛牆娲ｇ粭澶嬬節濞堝灝鏋熺紒鍝勬健瀹曠銇愰幒鎴犵暫濠德板€曢幊搴ｆ喆閿斿浜滈柡宥冨姀婢规鎮悢鍏尖拺?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <textarea id="cfg" spellcheck="false" placeholder="濠电姵顔栭崰妤冩崲閹邦喖绶ら柦妯侯檧閼版寧銇勮箛鎾搭棡閻庢碍宀搁幃褰掑炊閵娿儳绁风紓?frpc.toml..."></textarea>
    <div class="btns">
      <button class="btn ok" onclick="saveOnly()">婵犵數鍎戠徊钘壝洪敂鐐床闁稿瞼鍋為崑?/button>
      <button class="btn secondary" onclick="loadConfig()">闂傚倸鍊烽悞锕併亹閸愵亞鐭撻柣銏㈩焾閽冪喎鈹戦悩鎻掝仾閻庢碍宀搁幃褰掑炊閵娿儳绁风紓?/button>
    </div>
    <pre id="cfgLog">闂傚倷绀侀幉锟犲垂闂堟党娑樜旈崥钘夋喘椤㈡宕掑▎鎴濆⒕闂佸搫顦悧鍐疾濞戞鏆﹂柡鍥ュ灪閻?/pre>
  </div>
</div>

<div id="updateView" class="hidden">
  <div class="card">
    <div class="toolbar"><div><h3>闂傚倷绀侀幖顐⒚洪妶澶嬪仱闁靛ň鏅涢拑?frpc</h3><div class="sub">闂傚倷鑳剁划顖炲礉濡ゅ懎绠犻柟鎹愵嚙閸氳銇勯弴妤€浜鹃梺鎼炲妽閸庢娊鈥﹂妸鈺佺妞ゅ繐妫寸槐鍗炩攽閻樻剚鍟忛柛鐘崇墵椤㈡牠宕卞鏇熸そ瀹曠螖閳ь剙螞濮椻偓閺岀喖宕归锝囦紘闂佺顑嗛幐鎼佸煘閹达箑骞㈡俊顖濄€€閸嬫捇妫冨ù銏犵秺瀹曟鎳栭埡鍐ㄥ綆缂傚倷鐒﹂〃鍛村箠韫囨搩鐒芥い蹇撶墛鐎电姴顭跨捄娲€楅柣婵愬灦濮婃椽宕崟顒佹倷闂佸鏉垮闁?frpc.exe闂?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <div class="btns">
      <button class="btn ok" onclick="startUpdate()">闂佽瀛╅鏍窗閹烘纾婚柟鐐灱閺€鑺ャ亜閺冨倵鎷￠柛搴㈠姈缁绘盯骞撻幒鎾充淮閻?/button>
      <button class="btn secondary" onclick="showUpdateLog()">闂傚倷绀侀幖顐ゆ偖椤愶箑纾块柛鎰嚋閼?update.log</button>
    </div>
    <pre id="updateBox">Ready.</pre>
  </div>
</div>

<div id="logsView" class="hidden">
  <div class="card">
    <div class="toolbar"><div><h3 id="logTitle">闂傚倷绀侀幖顐﹀疮閵娾晛纾块弶鍫氭櫆瀹?/h3><div class="sub">闂傚倷绀侀幉锟犳偡椤栨稓顩叉繝闈涙焾閻旂绶炵€光偓閳ь剟鎯岄崱娑欑厽闁瑰鍊栭幋鐐殿浄婵せ鍋撻柡?260 闂備浇宕甸崑鐐电矙閹寸姵鍋栨い鎰剁畱閻?/div></div><button class="btn secondary" onclick="showHome()">闂備礁鎼ˇ顐﹀疾濠婂牆钃熼柕濞垮剭?/button></div>
    <div class="btns logtabs">
      <button class="btn secondary" onclick="showLogs('service')">service.log</button>
      <button class="btn secondary" onclick="showLogs('frpc')">frpc.log</button>
      <button class="btn secondary" onclick="showLogs('web')">web.log</button>
      <button class="btn warn" onclick="clearCurrentLog()">濠电姷鏁搁崑鐐哄箰閹间礁绠犻柟鐗堟緲缁犳牕鈹戦悩鍙夊櫡濞存粌缍婇弻鐔煎箚瑜嶉弳杈ㄣ亜閵堝懏鍤囬柡宀嬬節瀹曟﹢鏁冮埀顒勫礉濮樿京纾?/button>
    </div>
    <pre id="logBox">濠电姵顔栭崰妤冩崲閹邦喖绶ら柦妯侯檧閼版寧銇勮箛鎾搭棡閻庢碍宀搁幃褰掑炊閵娿儳绁风紓浣瑰絻濞硷繝寮婚敓鐘茬闁挎洍鍋撻柛鏃€姘ㄧ槐?..</pre>
  </div>
</div>

</div>
<script>
let closing = false;
let currentLog = 'service';
let authMode = 'login';
let currentUser = '';
function byId(id){return document.getElementById(id);} 
function setMain(s){ const el = byId('mainLog'); if(el){ el.textContent = s || ''; } } 
function setCfgLog(s){byId('cfgLog').textContent = s || '';} 
function setAuthLog(s){byId('authLog').textContent = s || '';} 
function setAccountLog(s){byId('accountLog').textContent = s || '';} 
function api(path,opt){
  opt = opt || {};
  return fetch(path, opt).then(async r => {
    const txt = await r.text();
    let data;
    try { data = JSON.parse(txt); } catch(e) { data = {ok:false,message:txt}; }
    if(!r.ok || data.ok === false){ throw new Error(data.message || data.output || txt || ('HTTP ' + r.status)); }
    return data;
  });
}
function showOnly(id){
  ['homeView','authView','accountView','basicView','proxiesView','configView','updateView','logsView'].forEach(x => byId(x).classList.add('hidden'));
  byId(id).classList.remove('hidden');
  byId('appHeader').classList.toggle('hidden', id === 'authView');
  byId('appWrap').classList.toggle('auth-mode', id === 'authView');
}
function showHome(){ showOnly('homeView'); refreshStatus(false); }

function showAuth(mode){
  authMode = mode || 'login';
  showOnly('authView');
  byId('authHint').textContent = authMode === 'setup' ? '婵犵妲呴崑鎾跺緤妤ｅ啯鍋嬫俊銈呮媼閺佸啴鏌涜箛鎾虫倯濠殿垱鎸抽幃褰掑箒閹烘垵顬夐梺鍝勬缁绘﹢骞冭ぐ鎺戠倞鐟滃秴危婵犳碍鐓曟俊顖濆吹閵嗘帡鏌熷畡閭﹀剶鐎规洘绮忛ˇ鎾煛鐎ｎ亜鈧潡寮婚敐澶涚稏妞ゆ巻鍋撳┑鈥茬矙閺屾盯鍩￠崒婊勫垱閻庤娲忛崕閬嶎敇婵傜宸濇い鏃傗拡娴煎懘姊虹拠鏌ュ弰婵炰匠鍏炬稑鈽夐姀鈥充函濠电姴锕ら悧鍡氱箽? : '闂備浇宕垫慨鏉懨洪妸鈺佽摕濠㈣泛鏈鑺ャ亜閹惧崬鐏╃紒鈧崒鐐寸厪濠㈣鍨扮€氼厼鈻撻姀銈嗏拺閻熸瑥瀚紞鎴︽煕閵忥紕鍙€妤犵偛鍟村鎾閻樼偨浠㈤梺璇茬箳閸嬬喖寮查鈶斤綁寮撮姀锛勫幗濡炪倖鏌ㄩ…宄邦瀶閻戣姤鐓?;
  byId('authButton').textContent = authMode === 'setup' ? '闂備浇宕垫慨宕囩矆娴ｈ娅犲ù鐘差儐閸嬵亪鏌涢埄鍐€掔€规挷绶氶弻娑㈠箻閼碱剙濡藉銈呯箞閸ㄥ骞? : '闂傚倷娴囬惃顐﹀幢閳轰焦顔勭紓?;
  setAuthLog('');
  byId('authPass').onkeydown = e => { if(e.key === 'Enter'){ submitAuth(); } };
  byId('authUser').focus();
}
function initAuth(){
  return api('/api/auth-state').then(d => {
    currentUser = d.username || '';
    if(!d.configured){ showAuth('setup'); return; }
    if(!d.loggedIn){ showAuth('login'); return; }
    byId('accountUser').value = currentUser;
    showHome();
  }).catch(e => {
    showAuth('login');
    setAuthLog('闂備浇宕垫慨鏉懨洪埡鍜佹晪鐟滄垿濡甸幇鏉跨倞妞ゆ帊绀侀崜顒勬煟閻樺弶绌块悘蹇旂懅缁棃宕稿Δ浣哄幗濠碘槅鍨伴悘婵嬫偂閹扮増鐓冪憸婊堝礂濞戞氨鐭嗗〒姘ｅ亾濠碉紕鏁婚獮瀣偐閸愬樊浼? ' + e.message);
  });
}
function submitAuth(){
  const username = byId('authUser').value;
  const password = byId('authPass').value;
  if(!username || !password){ setAuthLog('闂備浇宕垫慨鏉懨洪妸鈺佽摕濠㈣泛鏈鑺ャ亜閹惧崬鐏╃紒鈧崒鐐寸厪濠㈣鍨扮€氼厼鈻撻姀銈嗏拺閻熸瑥瀚紞鎴︽煕閵忥紕鍙€妤犵偛鍟村鎾閻樼偨浠㈤梺璇茬箳閸嬬喖寮查鈶斤綁寮撮姀锛勫幗濡炪倖鏌ㄩ…宄邦瀶閻戣姤鐓?); return; }
  const path = authMode === 'setup' ? '/api/setup' : '/api/login';
  setAuthLog(authMode === 'setup' ? '濠电姵顔栭崰妤冩崲閹邦喖绶ら柦妯侯檧閼版寧銇勮箛鎾搭棡濠殿垰銈搁弻娑㈠箻濡も偓閹冲繘鎮楅銏＄厽闁绘ê鍟挎慨褏绱掗悩鍐茬伇闁?..' : '濠电姵顔栭崰妤冩崲閹邦喖绶ら柦妯侯檧閼版寧銇勮箛鎾跺闁稿骸鐭傞幃褰掑炊椤忓嫮姣㈢紓?..');
  return api(path,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:username,password:password})})
    .then(d => { currentUser = d.username || username; byId('authPass').value=''; byId('accountUser').value=currentUser; showHome(); })
    .catch(e => setAuthLog(e.message));
}
function showAccount(){
  byId('accountUser').value = currentUser || '';
  byId('accountCurrent').value = '';
  byId('accountNew').value = '';
  byId('accountConfirm').value = '';
  setAccountLog('Ready.');
  showOnly('accountView');
}
function saveAccount(){
  const username = byId('accountUser').value;
  const currentPassword = byId('accountCurrent').value;
  const newPassword = byId('accountNew').value;
  const confirmPassword = byId('accountConfirm').value;
  if(!currentPassword){ setAccountLog('闂備浇宕垫慨鏉懨洪妸鈺佽摕濠㈣泛鏈鑺ャ亜閹惧崬鐏╃紒鈧崒鐐寸厪濠㈣泛鐗嗛崝銈囩磼婢跺鍋㈤柡灞剧洴楠炴ê鐣烽崶鍡愬灲閺岋綁鏁傞挊澶屼紝闂佽鍨扮粔鐢稿箯閸涱垰鏋堟俊顖濇〃婢?); return; }
  if(newPassword !== confirmPassword){ setAccountLog('婵犵數鍋為崹鍫曞箰閸洖纾归柡宥庡弾閺佸啴鏌涜箛鎾虫倯缂傚秴娲弻鐔煎箚瑜嶉弳杈┾偓娈垮枟婵炲﹪寮婚敐澶娢╅柕澶堝労娴犻箖姊洪崫鍕闁挎洩绠撻崺鈧い鎺戝濞懷勪繆椤愶絿娲撮柟顔哄劚椤劑宕熼娑欑亙闁诲骸绠嶉崕杈┾偓姘煎櫍椤㈡棃鏌嗗鍡欏幘闂備礁鐏濋鍥汲濮椻偓閺?); return; }
  setAccountLog('Saving...');
  return api('/api/account',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:username,currentPassword:currentPassword,newPassword:newPassword})})
    .then(d => { currentUser = d.username || username; setAccountLog('闂備浇宕垫慨鐢稿礉瑜忕划濠氬箣閿濆啩姹楅柣鐘充航閸斿秵鍒婃總鍛婄厱闁圭偓顨呴幊蹇涙倵椤撱垺鍊甸悷娆忓閹藉啰鈧鍠楅幐楣冨箲閵忕媭娼╅柟鍏哥娴滅偓绻涢幋鐑嗕紕婵犲﹤鐗嗛悞?); byId('accountCurrent').value=''; byId('accountNew').value=''; byId('accountConfirm').value=''; })
    .catch(e => setAccountLog('婵犵數鍎戠徊钘壝洪敂鐐床闁稿瞼鍋為崑銈夋煏婵犲繒鐣辩紒鍓佸仱瀵爼宕奸顫嚱閻? ' + e.message));
}
function logout(){
  return api('/api/logout',{method:'POST'}).then(() => {
    currentUser = '';
    showAuth('login');
  }).catch(e => alert('闂傚倸鍊风欢锟犲磻閳ь剟鏌涚€ｎ偅宕岄柡灞剧洴楠炲鏁愰崱鈺€鍝楁繝鐢靛仜閹冲矂宕曢幘顔嘉﹂柟鐗堟緲閸楁娊鏌ｉ幇鍏哥胺闁哄鍙冮弻? ' + e.message));
}

function setBasicLog(s){byId('basicLog').textContent = s || '';} 
function setProxyLog(s){byId('proxyLog').textContent = s || '';} 
function showBasic(){
  showOnly('basicView');
  setBasicLog('Loading...');
  return api('/api/basic').then(d => {
    byId('basicServerAddr').value = d.serverAddr || '';
    byId('basicServerPort').value = d.serverPort || ''; 
    byId('basicUser').value = d.user || '';
    byId('basicDns').value = d.dnsServer || '';
    byId('basicToken').value = d.token || '';
    byId('basicLogLevel').value = d.logLevel || 'info';
    byId('basicLogDays').value = d.logMaxDays || '7';
    setBasicLog('Loaded.');
  }).catch(e => setBasicLog('Load failed: ' + e.message));
}
function saveBasic(){
  setBasicLog('Saving...');
  const body = {
    serverAddr: byId('basicServerAddr').value,
    serverPort: byId('basicServerPort').value,
    user: byId('basicUser').value,
    dnsServer: byId('basicDns').value,
    token: byId('basicToken').value,
    logLevel: byId('basicLogLevel').value,
    logMaxDays: byId('basicLogDays').value
  };
  return api('/api/basic-save',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
    .then(d => { setBasicLog(d.message || 'Saved.'); return refreshStatus().then(() => showOnly('homeView')); })
    .catch(e => setBasicLog('Save failed: ' + e.message));
}
function syncProxyFields(){
  const t = byId('proxyType').value;
  const isDomain = (t === 'http' || t === 'https');
  byId('domainBox').classList.toggle('hidden', !isDomain);
  byId('remotePortBox').classList.toggle('hidden', isDomain);
}
function escapeHtml(s){return String(s||'').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));}
function renderProxyList(items){
  const box = byId('proxyList');
  if(!items || !items.length){ box.innerHTML = '<div class="small">闂佽崵鍠愮划搴㈡櫠濡ゅ懎绠伴柛娑橈攻濞呯娀鏌ｅΟ鑽ゃ偞闁哄矉绠撻弻宥夊煛娴ｅ憡娈茬紓浣哄У鐢帡婀侀梺缁樼懃閹虫劙骞冩總鍛婄厵妞ゆ梻銆嬮煬顒€鈹?/div>'; return; }
  box.innerHTML = items.map(p => {
    const meta = [p.type, 'local ' + (p.localIP || '') + ':' + (p.localPort || ''), p.remotePort ? ('remote ' + p.remotePort) : '', p.customDomains ? ('domain ' + p.customDomains) : ''].filter(Boolean).join(' / ');
    return '<div class="proxy-item"><div class="toolbar"><div><div class="proxy-title">#' + p.index + ' ' + escapeHtml(p.name || '(no name)') + '</div><div class="proxy-meta">' + escapeHtml(meta) + '</div></div><button class="btn bad" onclick="deleteProxy(' + p.index + ')">闂傚倷绀侀幉锛勬暜閻愬绠鹃柍褜鍓氱换?/button></div></div>';
  }).join('');
}
function showProxies(){
  showOnly('proxiesView');
  setProxyLog('Loading...');
  syncProxyFields();
  return api('/api/proxies').then(d => { renderProxyList(d.proxies || []); setProxyLog('Loaded.'); }).catch(e => setProxyLog('Load failed: ' + e.message));
}
function addProxy(){
  setProxyLog('Adding...');
  const body = { type:byId('proxyType').value, name:byId('proxyName').value, localIP:byId('proxyLocalIP').value, localPort:byId('proxyLocalPort').value, remotePort:byId('proxyRemotePort').value, customDomain:byId('proxyDomain').value };
  return api('/api/proxy-add',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
    .then(d => { setProxyLog(d.message || 'Added.'); byId('proxyName').value=''; byId('proxyLocalPort').value=''; byId('proxyRemotePort').value=''; byId('proxyDomain').value=''; return refreshStatus().then(() => showOnly('homeView')); })
    .catch(e => setProxyLog('Add failed: ' + e.message));
}
function deleteProxy(index){
  if(!confirm('Delete proxy #' + index + '?')) return;
  setProxyLog('Deleting...');
  return api('/api/proxy-delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({index:index})})
    .then(d => { setProxyLog(d.message || 'Deleted.'); return refreshStatus().then(() => showOnly('homeView')); })
    .catch(e => setProxyLog('Delete failed: ' + e.message));
}
function refreshStatus(verbose){
  const el = byId('runStatus');
  return api('/api/status').then(d => {
    if(d.status === 'running'){ el.textContent = 'frpc 闂備礁鎼ˇ顐﹀疾濠婂牆绀夋慨妞诲亾闁靛棔绶氶獮瀣倷绾版ɑ鐏?; el.className = 'header-status status-ok'; }
    else if(d.status === 'stopped'){ el.textContent = 'frpc 闂佽娴烽幊鎾诲箟闄囬妵鎰板礃閳哄喚娴勯梺缁樏Ο濠傤焽?; el.className = 'header-status status-bad'; }
    else { el.textContent = 'frpc 闂傚倷绀侀幖顐︽偋濠婂嫮顩查柨婵嗘处瀹曡尙鈧箍鍎卞ú锕傚磻?; el.className = 'header-status status-bad'; }
    if(verbose){ setMain(d.output || d.status); }
    return d;
  }).catch(e => { el.textContent = 'frpc 闂傚倷鑳剁划顖炩€﹂崼銉ユ槬闁哄稁鍘奸悞鍨亜閹达絾纭堕柛鏂跨У缁绘繈鍩€椤掑嫷鏁囬柕蹇娾偓鍐插?; el.className = 'header-status status-warn'; if(verbose){ setMain('Query failed: ' + e.message); } });
}
function waitStatus(target, timeoutMs){
  const end = Date.now() + (timeoutMs || 10000);
  function tick(){
    return refreshStatus(false).then(d => {
      if(!target || d.status === target || Date.now() >= end){ return d; }
      return new Promise(resolve => setTimeout(resolve, 350)).then(tick);
    });
  }
  return tick();
}
function ctl(action){
  const text = {start:'Starting frpc...',stop:'Stopping frpc...',restart:'Restarting frpc...',update:'Updating frpc. Download may take a while...'}[action] || 'Processing...';
  setMain(text);
  return api('/api/' + action,{method:'POST'}).then(d => {
    if(action === 'start' || action === 'stop'){
      setMain('');
    } else {
      setMain(d.output || d.message || 'Done');
    }
    const target = {start:'running',stop:'stopped',restart:'running'}[action];
    return waitStatus(target, 10000);
  }).catch(e => setMain('Failed: ' + e.message));
}
function lowMemory(){
  setMain('Starting frpc and closing this temporary Web server...');
  api('/api/low-memory',{method:'POST'}).then(d => {
    closing = true;
    document.body.innerHTML = '<div style="font-family:Microsoft YaHei UI,Segoe UI,Arial,sans-serif;padding:40px;text-align:center"><h2>Minimal-memory run enabled</h2><p>Start command has been sent. frpc keeps running. The Web backend is closing for minimal memory. You can close this tab.</p><pre style="text-align:left;display:inline-block;max-width:900px;white-space:pre-wrap">' + (d.output || d.message || '') + '</pre></div>';
  }).catch(e => setMain('Minimal-memory run failed: ' + e.message));
}

function showConfig(){
  showOnly('configView');
  loadConfig();
}
function loadConfig(){
  setCfgLog('Reading config...');
  return api('/api/config').then(d => { byId('cfg').value = d.content || ''; setCfgLog('Loaded: ' + d.path); }).catch(e => setCfgLog('Read failed: ' + e.message));
}
function saveOnly(){
  setCfgLog('Saving...');
  return api('/api/save',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({content:byId('cfg').value})})
    .then(d => setCfgLog(d.message || 'Config saved')).catch(e => setCfgLog('Save failed: ' + e.message));
}
function saveRestart(){
  return saveOnly();
}
function showUpdate(){
  showOnly('updateView');
  byId('updateBox').textContent = 'Ready.';
}
function startUpdate(){
  byId('updateBox').textContent = 'Updating...';
  return api('/api/update',{method:'POST'}).then(d => { byId('updateBox').textContent = d.output || d.message || 'Done'; refreshStatus(false); }).catch(e => { byId('updateBox').textContent = 'Update failed: ' + e.message; });
}
function showUpdateLog(){
  return api('/api/logs?which=update').then(d => { byId('updateBox').textContent = d.content || ''; }).catch(e => { byId('updateBox').textContent = 'Read failed: ' + e.message; });
}
function showLogs(which){
  currentLog = which || 'service';
  showOnly('logsView');
  byId('logTitle').textContent = currentLog + '.log';
  byId('logBox').textContent = 'Reading log...';
  return api('/api/logs?which=' + encodeURIComponent(currentLog)).then(d => {
    byId('logTitle').textContent = d.name || (currentLog + '.log');
    byId('logBox').textContent = d.content || '';
  }).catch(e => { byId('logBox').textContent = 'Read failed: ' + e.message; });
}
function clearCurrentLog(){
  return api('/api/log-clear?which=' + encodeURIComponent(currentLog || 'service'), {method:'POST'}).then(d => {
    byId('logBox').textContent = d.content || '';
  }).catch(e => { byId('logBox').textContent = 'Clear failed: ' + e.message; });
}
setInterval(()=>{ if(!closing){ fetch('/ping').catch(()=>{}); } }, 4000);
initAuth();
</script>
</body></html>`

//go:embed assets/index.html
var indexHTML string

const previousIndexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>frpc web console</title>
<style>
:root{--bg:#f8fafc;--card:#fff;--text:#0f172a;--muted:#64748b;--line:#e2e8f0;--primary:#2563eb;--ok:#16a34a;--bad:#dc2626;--warn:#d97706;--shadow:0 8px 24px rgba(15,23,42,.08)}
@media (prefers-color-scheme:dark){:root{--bg:#111827;--card:#1f2937;--text:#f3f4f6;--muted:#aeb6c2;--line:#374151;--primary:#60a5fa;--shadow:none}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font-family:"Microsoft YaHei UI","Segoe UI",system-ui,sans-serif;line-height:1.45}.wrap{max-width:960px;margin:0 auto;padding:16px}.wrap.auth-mode{max-width:none;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:0 18px 18vh;background:#f5f6fb}.card{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:16px;margin:14px 0;box-shadow:var(--shadow)}h1{font-size:24px;margin:4px 0 2px}h3{margin:0 0 10px}.sub{color:var(--muted);font-size:13px}.header-row{display:flex;justify-content:space-between;gap:12px;align-items:flex-start}.header-status{font-size:20px;font-weight:850;line-height:1.25;margin:4px 0 6px}.status-ok{color:var(--ok)}.status-bad{color:var(--bad)}.status-warn{color:var(--warn)}.btns{display:flex;flex-wrap:wrap;gap:10px;margin-top:8px}.btn{appearance:none;border:0;border-radius:12px;padding:11px 14px;text-decoration:none;background:var(--primary);color:#fff;font-weight:700;display:inline-block;cursor:pointer;font-size:14px}.btn.secondary{background:#64748b}.btn.ok{background:var(--ok)}.btn.bad{background:var(--bad)}.btn.warn{background:var(--warn)}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}.kv{padding:10px;border:1px solid var(--line);border-radius:12px}.k{color:var(--muted);font-size:12px}.hidden{display:none}input,select{width:100%;border:1px solid var(--line);border-radius:10px;padding:10px;background:var(--card);color:var(--text);font-size:14px}textarea{width:100%;min-height:62vh;border:1px solid var(--line);border-radius:12px;padding:12px;background:var(--card);color:var(--text);font-family:Consolas,"Cascadia Mono","Courier New",monospace;font-size:13px;line-height:1.55;outline:none}pre{white-space:pre-wrap;word-break:break-word;background:rgba(100,116,139,.12);border:1px solid var(--line);border-radius:12px;padding:12px;max-height:68vh;overflow:auto}.toolbar{display:flex;justify-content:space-between;gap:10px;align-items:center;flex-wrap:wrap}.logtabs .btn{padding:8px 11px;font-size:13px}.proxy-item{border:1px solid var(--line);border-radius:12px;padding:10px;margin:10px 0}.proxy-title{font-weight:800}.proxy-meta{color:var(--muted);font-size:12px;word-break:break-all}.auth-card{width:min(720px,100%);display:grid;grid-template-columns:220px minmax(0,1fr);column-gap:30px;align-items:center;background:#fff;border:1px solid #e2e8f0;border-radius:18px;padding:32px 30px 30px;box-shadow:0 18px 48px rgba(15,23,42,.08);position:relative}.auth-side h1{font-size:26px;line-height:1.2;margin:0;font-weight:800;color:#111827;letter-spacing:0}.auth-hint{margin-top:10px;color:#64748b;font-size:13px}.auth-lang{position:absolute;right:18px;top:16px;background:transparent;color:#2563eb;padding:4px 8px;font-size:13px}.auth-form{display:grid;gap:16px}.auth-field label{display:block;font-size:17px;line-height:1.25;font-weight:750;color:#111827;margin:0 0 9px}.auth-field input{height:41px;border-radius:10px;border:1px solid #dbe3f2;background:#eaf0ff;color:#111827;font-size:16px;padding:0 16px}.auth-submit{width:100%;height:37px;border-radius:10px;background:#3d63e6;font-size:15px;margin-top:4px;padding:0 14px}.auth-log{grid-column:2;margin:14px 0 0;min-height:0;max-height:120px;background:transparent;border:0;padding:0;color:#dc2626;font-size:13px}.auth-log:empty{display:none}
@media (max-width:640px){.auth-card{display:block;width:min(420px,100%)}.auth-side h1{margin:0 0 8px}.auth-hint{margin:0 0 18px}.auth-log{grid-column:auto}.header-row{display:block}.auth-lang{position:static;margin-bottom:14px}}
.modal{position:fixed;inset:0;background:rgba(15,23,42,.45);display:flex;align-items:center;justify-content:center;padding:18px;z-index:20}.modal.hidden{display:none}.modal-card{width:min(420px,100%);background:#fff;color:#111827;border-radius:16px;padding:16px;box-shadow:0 24px 60px rgba(15,23,42,.22)}.reward-img{display:block;width:100%;max-width:360px;margin:10px auto 0;border-radius:12px}.modal-card .toolbar{align-items:center}
</style>
</head>
<body><div class="wrap" id="appWrap">
<div class="card" id="appHeader"><div class="header-row"><div><h1 data-i18n="appTitle">frpc web console</h1><div class="header-status status-warn" id="runStatus">frpc checking...</div><div class="sub" data-i18n="appSub">Windows frpc web control panel.</div></div><button class="btn secondary" onclick="toggleLang()" id="langButton">婵炴垶鎼╅崢浠嬪几?/button></div></div>
<div id="homeView"><div class="card"><div class="btns"><button class="btn ok" onclick="ctl('start')" data-i18n="start">Start</button><button class="btn bad" onclick="ctl('stop')" data-i18n="stop">Stop</button><button class="btn warn" onclick="lowMemory()" data-i18n="lowMemory">Low-memory run</button></div></div><div class="card"><h3 data-i18n="manage">Management</h3><div class="btns"><button class="btn secondary" onclick="showBasic()" data-i18n="basic">Basic settings</button><button class="btn secondary" onclick="showProxies()" data-i18n="proxies">Proxy management</button><button class="btn secondary" onclick="showConfig()" data-i18n="config">Config file</button><button class="btn secondary" onclick="showLogs('service')" data-i18n="logs">Logs</button><button class="btn secondary" onclick="showUpdate()" data-i18n="update">Update frpc</button><button class="btn secondary" onclick="showAccount()" data-i18n="account">Account settings</button><button class="btn bad" onclick="logout()" data-i18n="logout">Logout</button></div></div></div>
<div id="authView" class="hidden"><div class="auth-card"><button class="btn auth-lang" onclick="toggleLang()" id="authLangButton">婵炴垶鎼╅崢浠嬪几?/button><div class="auth-side"><h1 data-i18n="appTitle">frpc web console</h1><div class="auth-hint" id="authHint"></div></div><div class="auth-form"><div class="auth-field"><label for="authUser" data-i18n="username">Username</label><input id="authUser" autocomplete="username"></div><div class="auth-field"><label for="authPass" data-i18n="password">Password</label><input id="authPass" type="password" autocomplete="current-password"></div><button class="btn auth-submit" onclick="submitAuth()" id="authButton">Login</button></div><pre id="authLog" class="auth-log"></pre></div></div>
<div id="accountView" class="hidden"><div class="card"><div class="toolbar"><div><h3 data-i18n="account">Account settings</h3><div class="sub" data-i18n="accountSub">Change the username or password used to enter this console.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><div class="grid"><div class="kv"><div class="k" data-i18n="username">Username</div><input id="accountUser" autocomplete="username"></div><div class="kv"><div class="k" data-i18n="currentPassword">Current password</div><input id="accountCurrent" type="password"></div><div class="kv"><div class="k" data-i18n="newPassword">New password</div><input id="accountNew" type="password"></div><div class="kv"><div class="k" data-i18n="confirmPassword">Confirm password</div><input id="accountConfirm" type="password"></div></div><div class="btns"><button class="btn ok" onclick="saveAccount()" data-i18n="saveAccount">Save account</button></div><pre id="accountLog">Ready.</pre></div></div>
<div id="basicView" class="hidden"><div class="card"><div class="toolbar"><div><h3 data-i18n="basic">Basic settings</h3><div class="sub" data-i18n="basicSub">Edit common frpc client options.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><div class="grid"><div class="kv"><div class="k">serverAddr</div><input id="basicServerAddr" placeholder="example.com or IP"></div><div class="kv"><div class="k">serverPort</div><input id="basicServerPort" inputmode="numeric"></div><div class="kv"><div class="k">user</div><input id="basicUser" placeholder="optional"></div><div class="kv"><div class="k">dnsServer</div><input id="basicDns" placeholder="223.5.5.5"></div><div class="kv"><div class="k">token</div><input id="basicToken" placeholder="optional"></div><div class="kv"><div class="k">log.level</div><input id="basicLogLevel" placeholder="info"></div><div class="kv"><div class="k">log.maxDays</div><input id="basicLogDays" inputmode="numeric" placeholder="7"></div></div><div class="btns"><button class="btn ok" onclick="saveBasic()" data-i18n="saveBasic">Save settings</button></div><pre id="basicLog">Ready.</pre></div></div>
<div id="proxiesView" class="hidden"><div class="card"><div class="toolbar"><div><h3 data-i18n="proxies">Proxy management</h3><div class="sub" data-i18n="proxiesSub">View and delete proxies from frpc.toml.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><div id="proxyList"></div></div><div class="card"><div class="toolbar"><div><h3 data-i18n="addProxy">Add proxy</h3><div class="sub" data-i18n="addProxySub">Supported types: tcp, udp, tcp + udp, http, https.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><div class="grid"><div class="kv"><div class="k" data-i18n="proxyType">Type</div><select id="proxyType" onchange="syncProxyFields()"><option value="tcpudp">tcp + udp</option><option value="tcp">tcp</option><option value="udp">udp</option><option value="http">http</option><option value="https">https</option></select></div><div class="kv"><div class="k" data-i18n="proxyName">Name</div><input id="proxyName" placeholder="ssh"></div><div class="kv"><div class="k">localIP</div><input id="proxyLocalIP" value="127.0.0.1"></div><div class="kv"><div class="k">localPort</div><input id="proxyLocalPort" inputmode="numeric" placeholder="22"></div><div class="kv" id="remotePortBox"><div class="k">remotePort</div><input id="proxyRemotePort" inputmode="numeric" placeholder="6000"></div><div class="kv hidden" id="domainBox"><div class="k">customDomains</div><input id="proxyDomain" placeholder="example.com"></div></div><div class="btns"><button class="btn ok" onclick="addProxy()" data-i18n="addProxy">Add proxy</button></div><pre id="proxyLog">Ready.</pre></div></div>
<div id="configView" class="hidden"><div class="card"><div class="toolbar"><div><h3 data-i18n="configTitle">Config file frpc.toml</h3><div class="sub" data-i18n="configSub">Edit the raw frpc.toml file directly.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><textarea id="cfg" spellcheck="false"></textarea><div class="btns"><button class="btn ok" onclick="saveOnly()" data-i18n="save">Save</button><button class="btn secondary" onclick="loadConfig()" data-i18n="reload">Reload</button></div><pre id="cfgLog">Ready.</pre></div></div>
<div id="updateView" class="hidden"><div class="card"><div class="toolbar"><div><h3 data-i18n="update">Update frpc</h3><div class="sub" data-i18n="updateSub">Download and replace frpc.exe.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><div class="btns"><button class="btn ok" onclick="startUpdate()" data-i18n="startUpdate">Start update</button><button class="btn secondary" onclick="showUpdateLog()" data-i18n="showUpdateLog">Show update.log</button></div><pre id="updateBox">Ready.</pre></div></div>
<div id="logsView" class="hidden"><div class="card"><div class="toolbar"><div><h3 id="logTitle">service.log</h3><div class="sub" data-i18n="logsSub">Display the latest log content.</div></div><button class="btn secondary" onclick="showHome()" data-i18n="back">Back</button></div><div class="btns logtabs"><button class="btn secondary" onclick="showLogs('service')">service.log</button><button class="btn secondary" onclick="showLogs('frpc')">frpc.log</button><button class="btn secondary" onclick="showLogs('web')">web.log</button><button class="btn bad" onclick="clearCurrentLog()" data-i18n="clearLog">Clear current log</button></div><pre id="logBox">Reading log...</pre></div></div>
</div>
<script>
let closing=false,currentLog='service',authMode='login',currentUser='',currentLang=localStorage.getItem('frpc_lang')||'en';
const i18n={en:{appTitle:'frpc web console',appSub:'Windows frpc web control panel.',langButton:'婵炴垶鎼╅崢浠嬪几?,start:'Start',stop:'Stop',lowMemory:'Low-memory run',manage:'Management',basic:'Basic settings',proxies:'Proxy management',config:'Config file',logs:'Logs',update:'Update frpc',account:'Account settings',close:'Close',logout:'Logout',back:'Back',username:'Username',password:'Password',login:'Login',setup:'Set account',loginHint:'Sign in to manage frpc.',setupHint:'First use: set a username and password.',needUserPass:'Please enter username and password.',signingIn:'Signing in...',settingAccount:'Setting account...',authInitFailed:'Auth state check failed: ',accountSub:'Change the username or password used to enter this console.',currentPassword:'Current password',newPassword:'New password',confirmPassword:'Confirm password',saveAccount:'Save account',needCurrent:'Please enter the current password.',passwordMismatch:'The new passwords do not match.',accountSaved:'Account saved.',saveFailed:'Save failed: ',logoutFailed:'Logout failed: ',basicSub:'Edit common frpc client options.',saveBasic:'Save settings',proxiesSub:'View and delete proxies from frpc.toml.',addProxy:'Add proxy',addProxySub:'Supported types: tcp, udp, tcp + udp, http, https.',proxyType:'Type',proxyName:'Name',configTitle:'Config file frpc.toml',configSub:'Edit the raw frpc.toml file directly.',save:'Save',reload:'Reload',updateSub:'Download and replace frpc.exe.',startUpdate:'Start update',showUpdateLog:'Show update.log',logsSub:'Display the latest log content.',clearLog:'Clear current log',ready:'Ready.',loading:'Loading...',loaded:'Loaded.',saving:'Saving...',saved:'Saved.',adding:'Adding...',added:'Added.',deleting:'Deleting...',deleted:'Deleted.',readingConfig:'Reading config...',readFailed:'Read failed: ',saveConfig:'Config saved',readingLog:'Reading log...',clearFailed:'Clear failed: ',running:'frpc running',stopped:'frpc stopped',unknown:'frpc status unknown',startText:'Starting frpc...',stopText:'Stopping frpc...',restartText:'Restarting frpc...',updateText:'Updating frpc. Download may take a while...',processing:'Processing...',failed:'Failed: ',emptyProxy:'No proxies found.',noName:'(no name)',deleteProxy:'Delete',deleteConfirm:'Delete proxy #',lowMemoryTitle:'Minimal-memory run enabled',lowMemoryBody:'Start command has been sent. frpc keeps running. The Web backend is closing for minimal memory. You can close this tab.',lowMemoryStart:'Starting frpc and closing this temporary Web server...',lowMemoryFailed:'Minimal-memory run failed: '},zh:{appTitle:'frpc web 闂佺鐭囬崘銊у幀闂?,appSub:'Windows frpc 缂傚倸鍟崹鍧楀Υ婢舵劕绠崇憸宥夊春濡ゅ懏顥堥柕蹇婂墲缁舵煡鏌?,langButton:'English',start:'闂佸憡鍑归崹鐗堟叏?,stop:'闂佺顑嗙划宥夘敆?,lowMemory:'婵炶揪绲芥鎼佸船鐎电硶鍋撳☉娆樼劸缂佽绉堕幃?,manage:'缂備胶濯寸槐鏇㈠箖?,basic:'闂佺硶鏅炲▍锝夈€侀崨顖涘闁告挆鍛€?,proxies:'婵炲濯寸徊鍧楀箖婵犲嫮涓嶉柨娑樺閸?,config:'闂備焦婢樼粔鍫曟偪閸℃稑妫橀柛銉檮椤?,logs:'闂佸搫鍟ㄩ崕杈╂崲?,update:'闂佸搫娲ら悺銊╁蓟?frpc',account:'闁荤姵鍔х粻鎴ｃ亹鐠恒劍濯奸柛鎾楀懏鐎?,reward:'闂佺懓鐏氶幑浣虹矈?,rewardTitle:'闂佺懓鐏氶幑浣虹矈婵犳碍鍎?,close:'闂佺绻戞繛濠偽?,logout:'闂備緡鍋€閸嬫捇鏌涢幋锝嗩仩婵炲懌鍎撮妵?,back:'闁哄鏅滈弻銊ッ?,username:'闂佹椿娼块崝宥夊春濞戙垹瑙?,password:'闁诲酣娼уΛ娑㈡偉?,login:'闂佽皫鍡╁殭缂?,setup:'闁荤姳绀佹晶浠嬫偪閸℃瑦瀚婚柨鏇楀亾鐟?,loginHint:'闂佽皫鍡╁殭缂傚秴绉瑰畷銉︽償閿濆骸鎮侀梺?frpc闂?,setupHint:'婵☆偓绲鹃悧妤咁敃閸忓吋濯撮悹鎭掑妽閺嗗繘鎮归崶鈺傜効妞ゆ梹娲滅槐鏃堫敊閻愵剚娈㈤梺瑙勬綑閸㈡煡骞冮弴銏犳そ閻忕偟鏅Σ鏇㈡煟椤斿妾烽柍?,needUserPass:'闁荤姴娲ㄩ弻澶屾椤撱垹绀傞柕澹嫭娈㈤梺瑙勬綑閸㈡煡骞冮弴銏犳そ閻忕偟鏅Σ鏇㈡煟椤斿妾烽柍?,signingIn:'濠殿喗绻愮徊钘夛耿椤忓牊鍎岄悹鍥皺缁?..',settingAccount:'濠殿喗绻愮徊钘夛耿椤忓棙濯奸柛鎾楀懏鐎柣鐘冲姧缁犳垼銇?..',authInitFailed:'濠碘槅鍋€閸嬫捇鏌＄仦璇插姢婵炲懌鍎撮妵鎰板即閻樻畫锕傛煙椤戣法顦﹂柕鍥ㄥ灩閹峰綊濡烽婊呯崶',accountSub:'婵烇絽娴傞崰妤呭极閸忓吋浜ゆ繛鎴灻鎶芥煙鐠ㄥ鍟悡鎴︽煕濞嗘搩鏆掑┑鐐叉喘閹粙濡歌閻ｉ亶鏌ｉ～顒€濡介柛鈺傜洴瀹曘儱顓奸崱妤冧粣闁诲酣娼уΛ娑㈡偉濠婂牆违?,currentPassword:'閻熸粎澧楅幐鍛婃櫠閻樼鍋撻棃娑欘棤闁?,newPassword:'闂佸搫鍊瑰姗€鎯侀幋锔藉剺?,confirmPassword:'缂佺虎鍙庨崰娑㈩敇閼姐倐鍋撻棃娑欘棤闁?,saveAccount:'婵烇絽娲︾换鍌炴偤閵娧勫闁挎洍鍋撶憸?,needCurrent:'闁荤姴娲ㄩ弻澶屾椤撱垹绀傞柕澶堝劤缁夊ジ鏌涢幘宕囆㈤柣锔藉灴閹秵鎷呯喊妯轰壕?,passwordMismatch:'婵炴垶鎸堕崐鏍敃閸忓吋缍囬柟鎯у暱瀵娊鏌ｉ妸銉ヮ仾闁哄苯锕﹂埀顒勬涧濡盯鎮ュ鍕枖鐎广儱瀚閬嶆煠闁垮鏆橀柍?,accountSaved:'闁荤姵鍔х粻鎴ｃ亹閸噮鍟呴棅顐幘缁犱粙鎮楀☉娅辨粓鍩€?,saveFailed:'婵烇絽娲︾换鍌炴偤閵婏箑绶為弶鍫亯琚濋梺?,logoutFailed:'闂備緡鍋€閸嬫捇鏌涢幋锝嗩仩婵炲懌鍎撮妵鎰板即閻愵亙鏉柣鐘冲姂閸庤崵妲?,basicSub:'缂傚倸鍊归悧鐐垫椤愩倖鏆滈柛婵勫劜閺?frpc 闁诲骸绠嶉崹娲春濞戞氨鍗氭い鏍亹閸嬫挻寰勯幇鈹惧亾瀹ュ违?,saveBasic:'婵烇絽娲︾换鍌炴偤閵娧勫闁告挆鍛€?,proxiesSub:'闂佸搫琚崕鍐诧耿閸涙潙妞介悘鐐舵閻忊晠姊?frpc.toml 婵炴垶鎼╅崢鎯р枔閹寸偟顩烽柨婵嗘处閸婄偤鏌?,addProxy:'濠电儑缍€椤曆勬叏閻愬顩烽柨婵嗘处閸?,addProxySub:'闂佽　鍋撴い鏍ㄧ☉閻?tcp闂侀潧妫旈懣鍢緋闂侀潧妫旀潏娓僷 + udp闂侀潧妫斿鎼晅p闂侀潧妫斿鎼晅ps闂?,proxyType:'缂備緡鍋夐褔鎮?,proxyName:'闂佸憡鑹剧粔鎯扳叿',configTitle:'闂備焦婢樼粔鍫曟偪閸℃稑妫橀柛銉檮椤?frpc.toml',configSub:'闂佺儵鏅涢悺銊ф暜鐎靛摜纾介柡宥庡墰鐢棝鏌涘Ο鐓庢瀻妞?frpc.toml 闂佸搫鍊稿ú锝呪枎閵忋倕违?,save:'婵烇絽娲︾换鍌炴偤?,reload:'闂備焦褰冪粔鐢稿蓟婵犲嫭瀚氶悹鍥ㄥ絻缁?,updateSub:'婵炴垶鎸搁鍫澝归崶顒傚祦闁肩鐏氱粋宀勬煙?frpc.exe闂?,startUpdate:'閻庢鍠掗崑鎾斥攽椤旂⒈鍎愭繛鎻掓健瀵?,showUpdateLog:'闂佸搫琚崕鍐诧耿?update.log',logsSub:'闂佸搫瀚晶浠嬪Φ濮樿泛瀚夐柍褜鍓熷顒勬偋閸噦绱氶棅顐㈡处椤ㄥ懘宕€电硶鍋撻悷鎵暛闁?,clearLog:'濠电偞鎸搁幊鎰板煘閺嶎兙浜归柟鎯у暱椤ゅ懘鏌￠崘锕€鍔氱紒?,ready:'Ready.',loading:'濠殿喗绻愮徊钘夛耿椤忓牆绀夐柣妯煎劋缁?..',loaded:'閻庣懓鎲¤ぐ鍐╂叏閻愬瓨濮滃┑顔藉姀閸?,saving:'濠殿喗绻愮徊钘夛耿椤忓懐鈹嶆繝闈涙閹?..',saved:'閻庤鐡曞鎾舵崲濮樻墎鍋撳☉娅辨粓鍩€?,adding:'濠殿喗绻愮徊钘夛耿椤忓媱搴ｆ嫚閹绘帩娼?..',added:'閻庡湱顭堝鍫曞锤婵犲洤绀夐柣姗嗗枓閸?,deleting:'濠殿喗绻愮徊钘夛耿椤忓牆绀嗛柣妯肩帛閻?..',deleted:'閻庣懓鎲¤ぐ鍐垂瑜版帗鈷旈柕鍫濆暊閸?,readingConfig:'濠殿喗绻愮徊钘夛耿椤忓棙瀚氶悹鍥ㄥ絻缁插潡姊洪弶璺ㄐら柣?..',readFailed:'闁荤姴娲╅褑銇愰崶銊ョ窞閺夊牜鍋夎闂?,saveConfig:'闂備焦婢樼粔鍫曟偪閸℃鍟呴棅顐幘缁犱粙鎮?,readingLog:'濠殿喗绻愮徊钘夛耿椤忓棙瀚氶悹鍥ㄥ絻缁插潡鏌￠崘锕€鍔氱紒?..',clearFailed:'濠电偞鎸搁幊鎰板煘閺嶃劌绶為弶鍫亯琚濋梺?,running:'frpc 闁哄鏅滈崝姗€銆侀幋鐐碘枖?,stopped:'frpc 閻庣懓鎲¤ぐ鍐╃閻樺樊娼?,unknown:'frpc 闂佺粯顭堥崺鏍焵椤戣法鍔嶆繝鈧銏″剹?,startText:'濠殿喗绻愮徊钘夛耿椤忓牆瑙︽い鏍ㄨ壘琚?frpc...',stopText:'濠殿喗绻愮徊钘夛耿椤忓牆纾绘繝濠傚閸?frpc...',restartText:'濠殿喗绻愮徊钘夛耿椤忓牊鐓傜€广儱鎳忛崕?frpc...',updateText:'濠殿喗绻愮徊钘夛耿椤忓牆鍗抽悗娑櫳戦悡鈧?frpc闂佹寧绋戞總鏃傜箔閸涱喗濮滈柦妯侯槸鐠佹煡鏌ら崗鍛煓婵炴挸澧庨幉鐗堟媴妞嬪寒浼囨繛鏉戝悑閼归箖宕?..',processing:'婵犮垼娉涚€氼噣骞冩繝鍐枖?..',failed:'婵犮垺鍎肩划鍓ф喆閿曞倹鏅?,emptyProxy:'濠电偛澶囬崜婵嗭耿娓氣偓楠炲秹骞嗚閻撳倸霉閻欏懐鎮奸柟顔硷躬婵?,noName:'闂佹寧绋戦悧濠傦耿椤撱垹宸濋柦妯侯槹閸婃娊鏌?,deleteProxy:'闂佸憡甯炴繛鈧繛?,deleteConfirm:'缂佺虎鍙庨崰娑㈩敇婵犳艾绀嗛柣妯肩帛閻濈喎霉閻欏懐鎮奸柟?#',lowMemoryTitle:'閻庣懓鎲¤ぐ鍐箚鎼淬劍鍋ㄩ柕濞垮€楃粔鐢告煕閹邦剚鍣归柣掳鍔嶅濠氬箛椤栵絾鏂€',lowMemoryBody:'闂佸憡鍑归崹鐗堟叏閳哄懎宸濋柟瀛樺笚婵垻鈧懓鎲¤ぐ鍐亹閸岀偞鐒诲〒姘功缁€濉畆pc 婵炴潙鍚嬪銊╁箣妞嬪海纾兼い鎿勭磿缁犮儵鎮跺☉妯款潶闁逞屽墮閸婇攱寰勫澶婄睄?Web 闂佸憡鑹炬姝屻亹閺夋埈娼伴柨婵嗘噺闊剟鏌涜箛鏂库枙婵☆偅鎸抽弫宥囦沪閽樺顔夋繛瀵稿О閸庨亶宕ｈ濮婂顢橀悤浣稿Π婵＄偑鍊楅弫璇差焽娴兼潙违?,lowMemoryStart:'濠殿喗绻愮徊钘夛耿椤忓牆瑙︽い鏍ㄨ壘琚?frpc 濡ょ姷鍋犲▔娑㈠矗瑜斿濠氼敇濠靛棭妲婚梺?Web 闂佸憡鑹炬姝屻亹?..',lowMemoryFailed:'婵炶揪绲芥鎼佸船鐎电硶鍋撳☉娆樼劸缂佽绉堕幃鎵沪婵劒鏉柣鐘冲姂閸庤崵妲?}};
function byId(id){return document.getElementById(id)}function tr(k){return(i18n[currentLang]&&i18n[currentLang][k])||i18n.en[k]||k}function setText(id,s){const e=byId(id);if(e)e.textContent=s||''}function setMain(s){}function setCfgLog(s){setText('cfgLog',s)}function setAuthLog(s){setText('authLog',s)}function setAccountLog(s){setText('accountLog',s)}function setBasicLog(s){setText('basicLog',s)}function setProxyLog(s){setText('proxyLog',s)}
function applyLang(){document.documentElement.lang=currentLang==='zh'?'zh-CN':'en';document.title=tr('appTitle');document.querySelectorAll('[data-i18n]').forEach(e=>e.textContent=tr(e.getAttribute('data-i18n')));setText('langButton',tr('langButton'));setText('authLangButton',tr('langButton'));setText('authHint',authMode==='setup'?tr('setupHint'):tr('loginHint'));setText('authButton',authMode==='setup'?tr('setup'):tr('login'))}
function toggleLang(){currentLang=currentLang==='en'?'zh':'en';localStorage.setItem('frpc_lang',currentLang);applyLang();refreshStatus(false)}
function api(path,opt){opt=opt||{};return fetch(path,opt).then(async r=>{const txt=await r.text();let data;try{data=JSON.parse(txt)}catch(e){data={ok:false,message:txt}}if(!r.ok||data.ok===false)throw new Error(data.message||data.output||txt||('HTTP '+r.status));return data})}
function showOnly(id){['homeView','authView','accountView','basicView','proxiesView','configView','updateView','logsView'].forEach(x=>byId(x).classList.add('hidden'));byId(id).classList.remove('hidden');byId('appHeader').classList.toggle('hidden',id==='authView');byId('appWrap').classList.toggle('auth-mode',id==='authView');applyLang()}function showHome(){showOnly('homeView');refreshStatus(false)}
function showAuth(mode){authMode=mode||'login';showOnly('authView');setAuthLog('');byId('authPass').onkeydown=e=>{if(e.key==='Enter')submitAuth()};byId('authUser').focus()}
function initAuth(){applyLang();return api('/api/auth-state').then(d=>{currentUser=d.username||'';if(!d.configured){showAuth('setup');return}if(!d.loggedIn){showAuth('login');return}byId('accountUser').value=currentUser;showHome()}).catch(e=>{showAuth('login');setAuthLog(tr('authInitFailed')+e.message)})}
function submitAuth(){const username=byId('authUser').value,password=byId('authPass').value;if(!username||!password){setAuthLog(tr('needUserPass'));return}const path=authMode==='setup'?'/api/setup':'/api/login';setAuthLog(authMode==='setup'?tr('settingAccount'):tr('signingIn'));return api(path,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username,password})}).then(d=>{currentUser=d.username||username;byId('authPass').value='';byId('accountUser').value=currentUser;showHome()}).catch(e=>setAuthLog(e.message))}
function showAccount(){byId('accountUser').value=currentUser||'';byId('accountCurrent').value='';byId('accountNew').value='';byId('accountConfirm').value='';setAccountLog(tr('ready'));showOnly('accountView')}
function saveAccount(){const username=byId('accountUser').value,currentPassword=byId('accountCurrent').value,newPassword=byId('accountNew').value,confirmPassword=byId('accountConfirm').value;if(!currentPassword){setAccountLog(tr('needCurrent'));return}if(newPassword!==confirmPassword){setAccountLog(tr('passwordMismatch'));return}setAccountLog(tr('saving'));return api('/api/account',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username,currentPassword,newPassword})}).then(d=>{currentUser=d.username||username;setAccountLog(tr('accountSaved'));byId('accountCurrent').value='';byId('accountNew').value='';byId('accountConfirm').value=''}).catch(e=>setAccountLog(tr('saveFailed')+e.message))}
function logout(){return api('/api/logout',{method:'POST'}).then(()=>{currentUser='';showAuth('login')}).catch(e=>alert(tr('logoutFailed')+e.message))}
function showBasic(){showOnly('basicView');setBasicLog(tr('loading'));return api('/api/basic').then(d=>{byId('basicServerAddr').value=d.serverAddr||'';byId('basicServerPort').value=d.serverPort||'';byId('basicUser').value=d.user||'';byId('basicDns').value=d.dnsServer||'';byId('basicToken').value=d.token||'';byId('basicLogLevel').value=d.logLevel||'info';byId('basicLogDays').value=d.logMaxDays||'7';setBasicLog(tr('loaded'))}).catch(e=>setBasicLog(tr('readFailed')+e.message))}
function saveBasic(){setBasicLog(tr('saving'));const body={serverAddr:byId('basicServerAddr').value,serverPort:byId('basicServerPort').value,user:byId('basicUser').value,dnsServer:byId('basicDns').value,token:byId('basicToken').value,logLevel:byId('basicLogLevel').value,logMaxDays:byId('basicLogDays').value};return api('/api/basic-save',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(d=>{setBasicLog(d.message||tr('saved'));return refreshStatus().then(()=>showOnly('homeView'))}).catch(e=>setBasicLog(tr('saveFailed')+e.message))}
function syncProxyFields(){const t=byId('proxyType').value,isDomain=t==='http'||t==='https';byId('domainBox').classList.toggle('hidden',!isDomain);byId('remotePortBox').classList.toggle('hidden',isDomain)}function escapeHtml(s){return String(s||'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function renderProxyList(items){const box=byId('proxyList');if(!items||!items.length){box.innerHTML='<div class="small">'+escapeHtml(tr('emptyProxy'))+'</div>';return}box.innerHTML=items.map(p=>{const meta=[p.type,'local '+(p.localIP||'')+':'+(p.localPort||''),p.remotePort?('remote '+p.remotePort):'',p.customDomains?('domain '+p.customDomains):''].filter(Boolean).join(' / ');return '<div class="proxy-item"><div class="toolbar"><div><div class="proxy-title">#'+p.index+' '+escapeHtml(p.name||tr('noName'))+'</div><div class="proxy-meta">'+escapeHtml(meta)+'</div></div><button class="btn bad" onclick="deleteProxy('+p.index+')">'+escapeHtml(tr('deleteProxy'))+'</button></div></div>'}).join('')}
function showProxies(){showOnly('proxiesView');setProxyLog(tr('loading'));syncProxyFields();return api('/api/proxies').then(d=>{renderProxyList(d.proxies||[]);setProxyLog(tr('loaded'))}).catch(e=>setProxyLog(tr('readFailed')+e.message))}
function addProxy(){setProxyLog(tr('adding'));const body={type:byId('proxyType').value,name:byId('proxyName').value,localIP:byId('proxyLocalIP').value,localPort:byId('proxyLocalPort').value,remotePort:byId('proxyRemotePort').value,customDomain:byId('proxyDomain').value};return api('/api/proxy-add',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(d=>{setProxyLog(d.message||tr('added'));byId('proxyName').value='';byId('proxyLocalPort').value='';byId('proxyRemotePort').value='';byId('proxyDomain').value='';return refreshStatus().then(()=>showOnly('homeView'))}).catch(e=>setProxyLog(tr('failed')+e.message))}
function deleteProxy(index){if(!confirm(tr('deleteConfirm')+index+'?'))return;setProxyLog(tr('deleting'));return api('/api/proxy-delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({index})}).then(d=>{setProxyLog(d.message||tr('deleted'));return refreshStatus().then(()=>showOnly('homeView'))}).catch(e=>setProxyLog(tr('failed')+e.message))}
function refreshStatus(verbose){const el=byId('runStatus');return api('/api/status').then(d=>{if(d.status==='running'){el.textContent=tr('running');el.className='header-status status-ok'}else if(d.status==='stopped'){el.textContent=tr('stopped');el.className='header-status status-bad'}else{el.textContent=tr('unknown');el.className='header-status status-bad'}return d}).catch(e=>{el.textContent=tr('unknown');el.className='header-status status-warn'})}
function waitStatus(target,timeoutMs){const end=Date.now()+(timeoutMs||10000);function tick(){return refreshStatus(false).then(d=>{if(!target||d.status===target||Date.now()>=end)return d;return new Promise(r=>setTimeout(r,350)).then(tick)})}return tick()}
function ctl(action){return api('/api/'+action,{method:'POST'}).then(d=>waitStatus({start:'running',stop:'stopped',restart:'running'}[action],10000)).catch(e=>alert(tr('failed')+e.message))}
function lowMemory(){api('/api/low-memory',{method:'POST'}).then(d=>{closing=true;document.body.innerHTML='<div style="font-family:Microsoft YaHei UI,Segoe UI,Arial,sans-serif;padding:40px;text-align:center"><h2>'+escapeHtml(tr('lowMemoryTitle'))+'</h2><p>'+escapeHtml(tr('lowMemoryBody'))+'</p><pre style="text-align:left;display:inline-block;max-width:900px;white-space:pre-wrap">'+escapeHtml(d.output||d.message||'')+'</pre></div>'}).catch(e=>alert(tr('lowMemoryFailed')+e.message))}
function showConfig(){showOnly('configView');loadConfig()}function loadConfig(){setCfgLog(tr('readingConfig'));return api('/api/config').then(d=>{byId('cfg').value=d.content||'';setCfgLog(tr('loaded')+' '+d.path)}).catch(e=>setCfgLog(tr('readFailed')+e.message))}function saveOnly(){setCfgLog(tr('saving'));return api('/api/save',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({content:byId('cfg').value})}).then(d=>setCfgLog(d.message||tr('saveConfig'))).catch(e=>setCfgLog(tr('saveFailed')+e.message))}
function saveRestart(){return saveOnly()}function showUpdate(){showOnly('updateView');byId('updateBox').textContent=tr('ready')}function startUpdate(){byId('updateBox').textContent=tr('updateText');return api('/api/update',{method:'POST'}).then(d=>{byId('updateBox').textContent=d.output||d.message||tr('saved');refreshStatus(false)}).catch(e=>{byId('updateBox').textContent=tr('failed')+e.message})}function showUpdateLog(){return api('/api/logs?which=update').then(d=>{byId('updateBox').textContent=d.content||''}).catch(e=>{byId('updateBox').textContent=tr('readFailed')+e.message})}
function showLogs(which){currentLog=which||'service';showOnly('logsView');byId('logTitle').textContent=currentLog+'.log';byId('logBox').textContent=tr('readingLog');return api('/api/logs?which='+encodeURIComponent(currentLog)).then(d=>{byId('logTitle').textContent=d.name||(currentLog+'.log');byId('logBox').textContent=d.content||''}).catch(e=>{byId('logBox').textContent=tr('readFailed')+e.message})}function clearCurrentLog(){return api('/api/log-clear?which='+encodeURIComponent(currentLog||'service'),{method:'POST'}).then(d=>{byId('logBox').textContent=d.content||''}).catch(e=>{byId('logBox').textContent=tr('clearFailed')+e.message})}

setInterval(()=>{if(!closing){fetch('/ping').catch(()=>{})}},4000);initAuth();
</script>
</body></html>`
