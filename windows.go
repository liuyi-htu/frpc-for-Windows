//go:build windows

package main

import (
	"archive/zip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows/svc"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed assets/frpc.exe
var bundledFrpcExe []byte

type paths struct {
	Base         string
	Config       string
	Logs         string
	FrpcExe      string
	SelfExe      string
	Icon         string
	State        string
	Disabled     string
	WebOff       string
	Auth         string
	ListenConfig string
	ListenIP     string
	ListenPort   int
}

func newPaths(base string) paths {
	self, _ := os.Executable()
	return paths{
		Base:         base,
		Config:       filepath.Join(base, "frpc.toml"),
		Logs:         filepath.Join(base, "logs"),
		FrpcExe:      defaultFrpcExe(base),
		SelfExe:      self,
		Icon:         filepath.Join(base, "frpc-web.ico"),
		State:        filepath.Join(base, "frpc.json"),
		Disabled:     filepath.Join(base, "disabled"),
		WebOff:       filepath.Join(base, "web-disabled"),
		Auth:         filepath.Join(base, "frpc.json"),
		ListenConfig: filepath.Join(base, "frpc.json"),
		ListenIP:     defaultWebListenIP,
		ListenPort:   defaultWebPort,
	}
}

func defaultFrpcExe(base string) string {
	return filepath.Join(base, "frpc.exe")
}

func defaultBaseDir() string {
	if runtime.GOOS == "windows" {
		return `D:\Program Files\frpc-web`
	}
	return filepath.Join(".", "frpc-web")
}

type frpcState struct {
	Disabled    bool       `json:"disabled"`
	WebDisabled bool       `json:"webDisabled"`
	ListenIP    string     `json:"listenIP"`
	ListenPort  int        `json:"listenPort"`
	Auth        authConfig `json:"auth"`
}

func defaultFrpcState() frpcState {
	return frpcState{
		Disabled:    true,
		WebDisabled: false,
		ListenIP:    defaultWebListenIP,
		ListenPort:  defaultWebPort,
	}
}

func normalizeFrpcState(st *frpcState) {
	st.ListenIP = strings.TrimSpace(st.ListenIP)
	if st.ListenIP == "" {
		st.ListenIP = defaultWebListenIP
	}
	if st.ListenPort == 0 {
		st.ListenPort = defaultWebPort
	}
}

func readFrpcState(p paths) (frpcState, error) {
	st := defaultFrpcState()
	b, err := os.ReadFile(p.State)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return st, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("read frpc.json: %w", err)
	}
	normalizeFrpcState(&st)
	return st, nil
}

func writeFrpcState(p paths, st frpcState) error {
	normalizeFrpcState(&st)
	if net.ParseIP(st.ListenIP) == nil {
		return fmt.Errorf("invalid listen IP in frpc.json: %s", st.ListenIP)
	}
	if st.ListenPort < 1 || st.ListenPort > 65535 {
		return fmt.Errorf("invalid listen port in frpc.json: %d", st.ListenPort)
	}
	if err := os.MkdirAll(filepath.Dir(p.State), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.State, append(b, '\n'), 0600)
}

func ensureFrpcState(p *paths) error {
	st, err := readFrpcState(*p)
	if err != nil {
		return err
	}
	if net.ParseIP(st.ListenIP) == nil {
		return fmt.Errorf("invalid listen IP in frpc.json: %s", st.ListenIP)
	}
	if st.ListenPort < 1 || st.ListenPort > 65535 {
		return fmt.Errorf("invalid listen port in frpc.json: %d", st.ListenPort)
	}
	p.ListenIP = st.ListenIP
	p.ListenPort = st.ListenPort
	return writeFrpcState(*p, st)
}

func cleanupLegacyStateFiles(p paths) {
	for _, name := range []string{"disabled", "web-disabled", "auth.json", "web-listen.json"} {
		_ = os.Remove(filepath.Join(p.Base, name))
	}
}

type webListenConfig struct {
	ListenIP   string `json:"listenIP"`
	ListenPort int    `json:"listenPort"`
}

func readWebListenConfig(p *paths) error {
	st, err := readFrpcState(*p)
	if err != nil {
		return err
	}
	if net.ParseIP(st.ListenIP) == nil {
		return fmt.Errorf("invalid listen IP in frpc.json: %s", st.ListenIP)
	}
	if st.ListenPort < 1 || st.ListenPort > 65535 {
		return fmt.Errorf("invalid listen port in frpc.json: %d", st.ListenPort)
	}
	p.ListenIP = st.ListenIP
	p.ListenPort = st.ListenPort
	return nil
}

func writeWebListenConfig(p paths) error {
	st, err := readFrpcState(p)
	if err != nil {
		return err
	}
	st.ListenIP = strings.TrimSpace(p.ListenIP)
	st.ListenPort = p.ListenPort
	return writeFrpcState(p, st)
}

func ensureWebListenConfig(p *paths) error {
	return ensureFrpcState(p)
}

func webListenAddr(p paths) string {
	return net.JoinHostPort(p.ListenIP, strconv.Itoa(p.ListenPort))
}

func defaultWebListenAddr() string {
	return net.JoinHostPort(defaultWebListenIP, strconv.Itoa(defaultWebPort))
}

func webClientHost(p paths) string {
	host := strings.TrimSpace(p.ListenIP)
	if host == "" || host == "0.0.0.0" {
		return "127.0.0.1"
	}
	if host == "::" || host == "[::]" {
		return "::1"
	}
	return strings.Trim(host, "[]")
}

func webURL(p paths) string {
	return "http://" + net.JoinHostPort(webClientHost(p), strconv.Itoa(p.ListenPort)) + "/ui"
}

func webListenURL(p paths) string {
	return "http://" + net.JoinHostPort(strings.Trim(p.ListenIP, "[]"), strconv.Itoa(p.ListenPort)) + "/ui"
}

func webHealthURL(p paths) string {
	return "http://" + net.JoinHostPort(webClientHost(p), strconv.Itoa(p.ListenPort)) + "/api/health"
}

func applyListenOverride(p *paths, listen string) error {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return nil
	}
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) == nil {
		return fmt.Errorf("invalid listen IP: %s", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid listen port: %s", portText)
	}
	p.ListenIP = host
	p.ListenPort = port
	return nil
}

func requestedListenPaths(current paths, listenIP, listenPort string) (paths, bool, error) {
	next := current
	listenIP = strings.TrimSpace(listenIP)
	listenPort = strings.TrimSpace(listenPort)
	if listenIP == "" && listenPort == "" {
		return next, false, nil
	}
	if listenIP == "" {
		listenIP = current.ListenIP
	}
	if listenPort == "" {
		listenPort = strconv.Itoa(current.ListenPort)
	}
	if net.ParseIP(listenIP) == nil {
		return next, false, fmt.Errorf("invalid listen IP: %s", listenIP)
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil || port < 1 || port > 65535 {
		return next, false, fmt.Errorf("invalid listen port: %s", listenPort)
	}
	next.ListenIP = listenIP
	next.ListenPort = port
	return next, webListenAddr(next) != webListenAddr(current), nil
}

func listenRequested(listenIP, listenPort string) bool {
	return strings.TrimSpace(listenIP) != "" || strings.TrimSpace(listenPort) != ""
}

func ensureDirs(p paths) error {
	for _, d := range []string{p.Base, p.Logs} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	if len(appIcon) > 0 {
		_ = os.WriteFile(p.Icon, appIcon, 0644)
	}
	if err := ensureBundledFrpc(p); err != nil {
		return err
	}
	if err := ensureSwitchDefaults(p); err != nil {
		return err
	}
	if err := ensureWebListenConfig(&p); err != nil {
		return err
	}
	cleanupLegacyStateFiles(p)
	cleanupLegacyDownloadsDir(p)
	if _, err := os.Stat(p.Config); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(p.Config, []byte(defaultConfig()), 0644)
	}
	return nil
}

func ensureSwitchDefaults(p paths) error {
	st, err := readFrpcState(p)
	if err != nil {
		return err
	}
	return writeFrpcState(p, st)
}

func defaultConfig() string {
	return "serverAddr = \"\"\r\n" +
		"user = \"user\"\r\n" +
		"dnsServer = \"223.5.5.5\"\r\n\r\n" +
		"log.to = \"./logs/frpc.log\"\r\n" +
		"log.level = \"info\"\r\n" +
		"log.maxDays = 7\r\n\r\n" +
		"# \u793a\u4f8b\u4ee3\u7406\uff1a\u5c06\u672c\u673a 127.0.0.1:22 \u901a\u8fc7 tcp \u6620\u5c04\u5230\u670d\u52a1\u5668 remotePort 6000\r\n" +
		"# [[proxies]]\r\n" +
		"# name = \"ssh\"\r\n" +
		"# type = \"tcp\"\r\n" +
		"# localIP = \"127.0.0.1\"\r\n" +
		"# localPort = 22\r\n" +
		"# remotePort = 6000\r\n"
}

func logLine(p paths, name, msg string) {
	_ = os.MkdirAll(p.Logs, 0755)
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg + "\r\n"
	_ = appendFile(filepath.Join(p.Logs, name), []byte(line))
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func existsText(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "YES"
	}
	return "NO"
}

func switchStatePaths(path string) paths {
	base := filepath.Dir(path)
	p := newPaths(base)
	return p
}

func switchOn(path string) bool {
	p := switchStatePaths(path)
	st, err := readFrpcState(p)
	if err != nil {
		return true
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "web-disabled":
		return st.WebDisabled
	case "disabled":
		return st.Disabled
	default:
		return true
	}
}

func writeSwitch(path string, on bool) error {
	p := switchStatePaths(path)
	st, err := readFrpcState(p)
	if err != nil {
		return err
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "web-disabled":
		st.WebDisabled = on
	case "disabled":
		st.Disabled = on
	default:
		return fmt.Errorf("unknown switch file: %s", path)
	}
	return writeFrpcState(p, st)
}

func switchText(path string) string {
	if switchOn(path) {
		return "YES"
	}
	return "NO"
}

func frpcExeReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

func configReady(p paths) bool {
	if !frpcExeReady(p.FrpcExe) {
		return false
	}
	return configTextReady(p.Config)
}

func configTextReady(path string) bool {
	textBytes, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(textBytes)
	return strings.TrimSpace(tomlValue(text, "serverAddr")) != "" && strings.TrimSpace(tomlValue(text, "serverPort")) != ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func logPath(p paths, which string) (name string, path string) {
	m := map[string]string{
		"service": "service.log",
		"frpc":    "frpc.log",
		"update":  "update.log",
		"web":     "web.log",
	}
	file := m[which]
	if file == "" {
		file = "service.log"
	}
	return file, filepath.Join(p.Logs, file)
}

func migrateTruncatedBaseFiles(p paths) {
	if runtime.GOOS != "windows" || !strings.Contains(p.Base, `\Program Files\`) {
		return
	}
	legacyBase := strings.Replace(p.Base, `\Program Files\`, `\Program\`, 1)
	if samePath(legacyBase, p.Base) {
		return
	}
	legacy := newPaths(legacyBase)
	if !configTextReady(p.Config) && configTextReady(legacy.Config) {
		_ = copyFile(legacy.Config, p.Config)
	}
	if !authConfigured(p) && authConfigured(legacy) {
		_ = copyFile(legacy.Auth, p.Auth)
	}
}

func ensureBundledFrpc(p paths) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if len(bundledFrpcExe) == 0 {
		return nil
	}
	info, err := os.Stat(p.FrpcExe)
	if err == nil && info.Size() > 0 {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(p.FrpcExe, bundledFrpcExe, 0755); err != nil {
		return fmt.Errorf("write bundled frpc.exe: %w", err)
	}
	return nil
}

func install(p paths, noDownload bool) error {
	if err := ensureDirs(p); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	installedExe := filepath.Join(p.Base, "frpc-web.exe")
	if !samePath(self, installedExe) {
		if err := copyFile(self, installedExe); err != nil {
			return fmt.Errorf("copy exe to install dir: %w", err)
		}
		args := []string{"install-services", "--base", p.Base}
		if noDownload {
			args = append(args, "--no-download")
		}
		c := exec.Command(installedExe, args...)
		c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
		return c.Run()
	}
	return installServices(p, true)
}

func installFromUI(p paths, noDownload bool) error {
	if err := ensureDirs(p); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	installedExe := filepath.Join(p.Base, "frpc-web.exe")
	if !samePath(self, installedExe) {
		if err := copyFile(self, installedExe); err != nil {
			return fmt.Errorf("copy exe to install dir: %w", err)
		}
		args := []string{"install-services-ui", "--base", p.Base}
		if noDownload {
			args = append(args, "--no-download")
		}
		c := exec.Command(installedExe, args...)
		c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
		return c.Run()
	}
	return installServicesNoStart(p, true)
}

func installServices(p paths, createDesktopShortcut bool) error {
	return installServicesCore(p, true, createDesktopShortcut)
}

func installServicesNoStart(p paths, createDesktopShortcut bool) error {
	return installServicesCore(p, false, createDesktopShortcut)
}

func installServicesCore(p paths, startWeb bool, createDesktopShortcut bool) error {
	_ = startWeb // v8: frpc-web is no longer installed as a Windows service.
	if err := ensureDirs(p); err != nil {
		return err
	}
	if !frpcExeReady(p.FrpcExe) {
		_ = os.Remove(p.FrpcExe)
		fmt.Println("frpc.exe not found or empty, downloading latest release...")
		if _, err := updateFrpc(p, "latest"); err != nil {
			fmt.Println("frpc.exe download failed. You can copy frpc.exe manually later.")
			fmt.Println(err)
		}
	}
	_ = writeSwitch(p.WebOff, false)
	_ = writeSwitch(p.Disabled, false)

	if err := recreateService(p, serviceFrpc, "frpc", "Runs frpc.exe as a Windows service.", "run-frpc"); err != nil {
		return err
	}
	_ = createShortcutFiles(p, createDesktopShortcut)
	if configReady(p) {
		_ = controlService(serviceFrpc, "start")
	}
	fmt.Println("Install finished.")
	fmt.Println("InstallDir:", p.Base)
	fmt.Println("Config    :", p.Config)
	fmt.Println("Web       :", webURL(p))
	fmt.Println("Service   : frpc")
	return nil
}

func recreateService(p paths, name, display, desc, arg string) error {
	installedExe := filepath.Join(p.Base, "frpc-web.exe")
	bin := fmt.Sprintf("\"%s\" %s --base \"%s\"", installedExe, arg, p.Base)
	_ = controlService(name, "stop")
	_, _ = runSC("delete", name)
	time.Sleep(800 * time.Millisecond)
	if out, err := runSC("create", name, "binPath=", bin, "start=", "auto", "DisplayName=", display); err != nil {
		return fmt.Errorf("install service %s: %w\n%s", name, err, out)
	}
	_, _ = runSC("description", name, desc)
	_, _ = runSC("failure", name, "reset=", "60", "actions=", `restart/5000/restart/5000/""/0`)
	return nil
}

func uninstallServices(p paths) error {
	var msgs []string
	_ = controlService(serviceFrpc, "stop")
	out, err := runSC("delete", serviceFrpc)
	if err != nil && !strings.Contains(out, "1060") && !strings.Contains(strings.ToLower(out), "does not exist") {
		msgs = append(msgs, serviceFrpc+": "+out)
	} else {
		fmt.Println("uninstalled:", serviceFrpc)
	}
	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

func controlService(name, action string) error {
	if runtime.GOOS != "windows" {
		return controlOpenWrtService(name, action)
	}
	switch action {
	case "start":
		out, err := runSC("start", name)
		if err != nil && !strings.Contains(strings.ToUpper(out), "RUNNING") && !strings.Contains(out, "1056") {
			return fmt.Errorf("sc start %s: %w\n%s", name, err, out)
		}
		return nil
	case "stop":
		out, err := runSC("stop", name)
		if err != nil && !strings.Contains(out, "1062") && !strings.Contains(out, "1060") {
			return fmt.Errorf("sc stop %s: %w\n%s", name, err, out)
		}
		return nil
	case "restart":
		_ = controlService(name, "stop")
		time.Sleep(900 * time.Millisecond)
		return controlService(name, "start")
	default:
		return fmt.Errorf("unknown service action: %s", action)
	}
}

func runSC(args ...string) (string, error) {
	cmd := hiddenCommand("sc.exe", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func queryServiceStatus(name string) string {
	if runtime.GOOS != "windows" {
		if openWrtFrpcRunning() {
			return "RUNNING"
		}
		return "STOPPED"
	}
	out, err := runSC("query", name)
	if err != nil {
		if strings.Contains(out, "1060") || strings.Contains(strings.ToLower(out), "does not exist") {
			return "NOT INSTALLED"
		}
		return "UNKNOWN / " + strings.TrimSpace(out)
	}
	upper := strings.ToUpper(out)
	for _, s := range []string{"RUNNING", "STOPPED", "START_PENDING", "STOP_PENDING", "PAUSED"} {
		if strings.Contains(upper, s) {
			return s
		}
	}
	return strings.TrimSpace(out)
}

func restartWebWithListen(p paths) error {
	if err := writeSwitch(p.WebOff, false); err != nil {
		return err
	}
	self := p.SelfExe
	if self == "" {
		var err error
		self, err = os.Executable()
		if err != nil {
			return err
		}
	}
	if runtime.GOOS != "windows" {
		cmd := exec.Command(self, "open", "--base", p.Base)
		cmd.Dir = "/"
		return cmd.Start()
	}
	ps := "Start-Sleep -Milliseconds 1000; " +
		"Start-Process -FilePath '" + strings.ReplaceAll(self, "'", "''") + "' " +
		"-WorkingDirectory '" + strings.ReplaceAll(p.Base, "'", "''") + "' " +
		"-ArgumentList @('open','--base','" + strings.ReplaceAll(p.Base, "'", "''") + "')"
	cmd := hiddenCommand("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-Command", ps)
	return cmd.Start()
}

func createShortcutFiles(p paths, createDesktopShortcut bool) error {
	target := filepath.Join(p.Base, "frpc-web.exe")
	args := "open --base \"" + p.Base + "\""
	if dir := os.Getenv("PUBLIC"); dir != "" {
		desktop := filepath.Join(dir, "Desktop")
		removeOldShortcutNames(desktop)
		if createDesktopShortcut {
			_ = createWindowsShortcut(filepath.Join(desktop, "frpc-web.lnk"), target, args, p.Base, p.Icon)
		} else {
			_ = os.Remove(filepath.Join(desktop, "frpc-web.lnk"))
		}
	}
	programData := os.Getenv("ProgramData")
	if programData != "" {
		programs := filepath.Join(programData, "Microsoft", "Windows", "Start Menu", "Programs")
		_ = os.RemoveAll(filepath.Join(programs, "frpc-web"))
		removeOldShortcutNames(programs)
		_ = createWindowsShortcut(filepath.Join(programs, "frpc-web.lnk"), target, args, p.Base, p.Icon)
	}
	return nil
}

func removeOldShortcutNames(dir string) {
	for _, name := range []string{
		"frpc-web.url",
		"frpc-web 闂傚倷鑳堕、濠囨儗閸ヮ剙绀冮柕濞у啫绠戦梻?url",
		"frpc-web 闂傚倷鑳堕、濠囨儗閸ヮ剙绀冮柕濞у啫绠戦梻?lnk",
		"frpc-web 闂傚倷鑳堕、濠囨儗閸ヮ剙绀冮柕濞у啫绠戦梻?cmd",
		"frpc.url",
	} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func createWindowsShortcut(linkPath, target, args, workDir, iconPath string) error {
	ps := "$WshShell = New-Object -ComObject WScript.Shell;" +
		"$Shortcut = $WshShell.CreateShortcut('" + strings.ReplaceAll(linkPath, "'", "''") + "');" +
		"$Shortcut.TargetPath = '" + strings.ReplaceAll(target, "'", "''") + "';" +
		"$Shortcut.Arguments = '" + strings.ReplaceAll(args, "'", "''") + "';" +
		"$Shortcut.WorkingDirectory = '" + strings.ReplaceAll(workDir, "'", "''") + "';" +
		"$Shortcut.IconLocation = '" + strings.ReplaceAll(iconPath+",0", "'", "''") + "';" +
		"$Shortcut.Save()"
	return hiddenCommand("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps).Run()
}

func openWeb(p paths) error {
	if err := ensureDirs(p); err != nil {
		fallback, fallbackErr := fallbackOpenPaths(p, err)
		if fallbackErr != nil {
			return fallbackErr
		}
		p = fallback
	}
	// Double-click mode: frpc-web.exe itself hosts the local Web console.
	// Closing the browser window does not stop this process. Only the
	// "minimal-memory" action closes the Web backend.
	_ = writeSwitch(p.WebOff, false)
	addr := webListenAddr(p)
	url := webURL(p)
	if isPortOpen(addr) {
		if webBackendHealthy(p) {
			return openURL(url)
		}
		if isAdmin() {
			if err := killWebPortOwner(p); err != nil {
				return err
			}
			for i := 0; i < 20 && isPortOpen(addr); i++ {
				time.Sleep(250 * time.Millisecond)
			}
			if isPortOpen(addr) {
				return fmt.Errorf("\u7aef\u53e3 %s \u4ecd\u88ab\u5360\u7528\uff0c\u65e0\u6cd5\u542f\u52a8 Web \u540e\u53f0\u3002\u8bf7\u7ed3\u675f\u65e7\u7684 frpc-web.exe \u540e\u91cd\u8bd5\u3002", addr)
			}
		} else {
			if err := runElevated("recover-web", "--base", p.Base); err != nil {
				return err
			}
			return nil
		}
	}

	if isPortOpen(addr) {
		return openURL(url)
	}

	stop := make(chan struct{})
	sp := &svcProgram{paths: p, mode: "web", stop: stop}
	go sp.runWebServer()
	for i := 0; i < 50; i++ {
		if webBackendHealthy(p) {
			if err := openURL(url); err != nil {
				return err
			}
			select {}
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stop)
	return fmt.Errorf("Web 闂傚倷绀侀幉锟犳嚌閻愵剦娈芥慨婵嗚娴滃綊鏌熺紒銏犳灈閻熸瑱闄勯妵鍕冀閵娿劌顥濋悶姘卞枑缁绘繈濮€閳ュ啿濮哥紓渚囧枛婢т粙骞夐幘顔芥櫇闁稿本绋掑▍鏍倵閸忓浜鹃梺鍛婂姈閸庡啿鈻撻弻銉︹拺闁告繂瀚～锕傛煕鎼粹€虫毐闁?logs/web.log")
}

func recoverWeb(p paths) error {
	if err := ensureDirs(p); err != nil {
		return err
	}
	if !isAdmin() {
		return runElevated("recover-web", "--base", p.Base)
	}
	addr := webListenAddr(p)
	if isPortOpen(addr) && !webBackendHealthy(p) {
		if err := killWebPortOwner(p); err != nil {
			return err
		}
		for i := 0; i < 20 && isPortOpen(addr); i++ {
			time.Sleep(250 * time.Millisecond)
		}
	}
	return openWeb(p)
}

func killWebPortOwner(p paths) error {
	out, err := hiddenCommand("netstat.exe", "-ano").CombinedOutput()
	if err != nil {
		return fmt.Errorf("閺冪姵纭堕弻銉嚄缁旑垰褰?%s 閸楃姷鏁ゆ潻娑氣柤: %w", webListenAddr(p), err)
	}
	pids := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if strings.EqualFold(fields[0], "TCP") && localAddrHasPort(fields[1], p.ListenPort) && strings.EqualFold(fields[3], "LISTENING") {
			pids[fields[4]] = true
		}
	}
	if len(pids) == 0 {
		return nil
	}
	self := strconv.Itoa(os.Getpid())
	for pid := range pids {
		if pid == self || pid == "0" {
			continue
		}
		out, err := hiddenCommand("taskkill.exe", "/PID", pid, "/F").CombinedOutput()
		if err != nil {
			return fmt.Errorf("缂傚倸鍊搁崐鐑芥倿閿曞倸绠伴悹鍥ф▕閻掕姤銇勯幇鍓佺暠缂?Web 闂傚倷绀侀幉锟犳嚌閻愵剦娈芥慨婵嗚娴?PID %s 婵犵數濮伴崹娲磿閼测晛鍨濋柛鎾楀嫬鏋? %w\n%s", pid, err, string(out))
		}
	}
	return nil
}

func fallbackOpenPaths(p paths, originalErr error) (paths, error) {
	if runtime.GOOS != "windows" || !samePath(p.Base, defaultBaseDir()) {
		return p, originalErr
	}
	exe, err := os.Executable()
	if err != nil {
		return p, originalErr
	}
	fallback := newPaths(filepath.Dir(exe))
	if samePath(fallback.Base, p.Base) {
		return p, originalErr
	}
	if err := ensureDirs(fallback); err != nil {
		return p, fmt.Errorf("%w; fallback to %s also failed: %v", originalErr, fallback.Base, err)
	}
	return fallback, nil
}

func runAdminWeb(p paths) error {
	if err := ensureDirs(p); err != nil {
		return err
	}
	_ = writeSwitch(p.WebOff, false)
	// The non-admin Web process exits right after it launches this elevated
	// instance. Wait for 62930 to become free, then host the Web console here.
	addr := webListenAddr(p)
	url := webURL(p)
	for i := 0; i < 80; i++ {
		if !isPortOpen(addr) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if isPortOpen(addr) {
		_ = openURL(url)
		return fmt.Errorf("\u7aef\u53e3 %s \u4ecd\u88ab\u5360\u7528\uff0c\u7ba1\u7406\u5458 Web \u540e\u53f0\u672a\u80fd\u63a5\u7ba1\u3002\u8bf7\u5148\u7ed3\u675f\u65e7\u7684 frpc-web.exe\uff0c\u518d\u91cd\u65b0\u6253\u5f00\u3002", addr)
	}

	stop := make(chan struct{})
	sp := &svcProgram{paths: p, mode: "web", stop: stop}
	go sp.runWebServer()
	for i := 0; i < 40; i++ {
		if isPortOpen(addr) {
			if err := openURL(url); err != nil {
				return err
			}
			select {}
		}
		time.Sleep(250 * time.Millisecond)
	}
	close(stop)
	return fmt.Errorf("缂傚倸鍊烽懗鑸靛垔鐎靛憡顫曢柡鍥ュ灩缁犳牕鈹戦悩鍙夋悙鐎?Web 闂傚倷绀侀幉锟犳嚌閻愵剦娈芥慨婵嗚娴滃綊鏌熺紒銏犳灈閻熸瑱闄勯妵鍕冀閵娿劌顥濋悶姘卞枑缁绘繈濮€閳ュ啿濮哥紓渚囧枛婢т粙骞夐幘顔芥櫇闁稿本绋掑▍鏍倵閸忓浜鹃梺鍛婂姈閸庡啿鈻撻弻銉︹拺闁告繂瀚～锕傛煕鎼粹€虫毐闁?logs/web.log")
}

func openURL(u string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = hiddenCommand("rundll32", "url.dll,FileProtocolHandler", u)
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", u)
	} else {
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

func openFile(path string) error {
	if runtime.GOOS == "windows" {
		return exec.Command("notepad.exe", path).Start()
	}
	return openURL("file://" + path)
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func runAsService(p paths, mode string) error {
	name := map[string]string{"frpc": serviceFrpc, "web": serviceWeb}[mode]
	sp := &svcProgram{paths: p, mode: mode}
	_ = ensureDirs(p)
	return runWindowsService(name, func(stop <-chan struct{}) {
		sp.stop = stop
		sp.run()
	})
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func updateFrpc(p paths, version string) (result string, err error) {
	if err := ensureDirs(p); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(p.Logs, "update.log"), nil, 0644)
	var report []string
	add := func(msg string) {
		report = append(report, msg)
		logLine(p, "update.log", msg)
	}

	add("online update started")
	if blocked, status := frpcServiceBlocksUpdate(); blocked {
		add("frpc service status: " + status)
		add("update blocked: stop frpc service before updating")
		add("online update finished")
		return strings.Join(report, "\n"), nil
	}
	currentVersion := currentFrpcVersion(p)
	if currentVersion == "" {
		add("current version: unknown")
	} else {
		add("current version: " + currentVersion)
	}

	rel, err := getFrpRelease(version)
	if err != nil {
		add("failed: query latest version: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	latestVersion := normalizeFrpcVersion(rel.TagName)
	add("latest version: " + latestVersion)

	if currentVersion != "" && normalizeFrpcVersion(currentVersion) == latestVersion {
		add("already latest")
		cleanupLegacyDownloadsDir(p)
		cleanupOldFrpcBackups(p)
		add("online update finished")
		return strings.Join(report, "\n"), nil
	}

	ver := strings.TrimPrefix(latestVersion, "v")
	arch := windowsArch()
	expected := fmt.Sprintf("frp_%s_%s.zip", ver, arch)
	assetName, assetURL := "", ""
	for _, a := range rel.Assets {
		if a.Name == expected {
			assetName, assetURL = a.Name, a.URL
			break
		}
	}
	if assetURL == "" {
		for _, a := range rel.Assets {
			if strings.Contains(a.Name, arch) && strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
				assetName, assetURL = a.Name, a.URL
				break
			}
		}
	}
	if assetURL == "" {
		err := fmt.Errorf("no matching Windows asset found for %s", arch)
		add("failed: " + err.Error())
		return strings.Join(report, "\n"), err
	}

	tempDir, err := makeUpdateTempDir("online")
	if err != nil {
		add("failed: prepare temporary directory: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	defer removeAllWithRetry(tempDir)

	zipPath := filepath.Join(tempDir, assetName)
	extractDir := filepath.Join(tempDir, "extract")

	add("downloading package")
	if err := downloadFile(assetURL, zipPath); err != nil {
		add("failed: download: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	add("download complete")

	if err := unzip(zipPath, extractDir); err != nil {
		add("failed: extract package: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	add("extract complete")

	newExe, err := findFile(extractDir, "frpc.exe")
	if err != nil {
		add("failed: frpc.exe not found in package: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	candidateExe, err := makeFrpcUpdateCandidate(tempDir, newExe)
	if err != nil {
		add("failed: prepare frpc.exe: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	defer removeFileWithRetry(candidateExe)
	if err := replaceFrpcExe(p, candidateExe, latestVersion, add); err != nil {
		return strings.Join(report, "\n"), err
	}

	cleanupUpdateTempDir(tempDir)
	tempDir = ""
	add("online update finished")
	return strings.Join(report, "\n"), nil
}

func updateFrpcFromUpload(p paths, r *http.Request) (result string, err error) {
	if err := ensureDirs(p); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(p.Logs, "update.log"), nil, 0644)
	var report []string
	add := func(msg string) {
		report = append(report, msg)
		logLine(p, "update.log", msg)
	}

	add("local update started")
	if blocked, status := frpcServiceBlocksUpdate(); blocked {
		add("frpc service status: " + status)
		add("update blocked: stop frpc service before uploading")
		add("local update finished")
		return strings.Join(report, "\n"), nil
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		add("failed: read upload: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		add("failed: upload file not found: " + err.Error())
		return strings.Join(report, "\n"), err
	}

	tempDir, err := makeUpdateTempDir("upload")
	if err != nil {
		_ = file.Close()
		add("failed: prepare temporary directory: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	defer removeAllWithRetry(tempDir)

	fileName := filepath.Base(strings.TrimSpace(header.Filename))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) {
		fileName = "frpc-upload"
	}
	uploadPath := filepath.Join(tempDir, fileName)
	out, err := os.Create(uploadPath)
	if err != nil {
		_ = file.Close()
		add("failed: save upload: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	written, copyErr := io.Copy(out, file)
	closeOutErr := out.Close()
	closeInErr := file.Close()
	if copyErr != nil {
		add("failed: save upload: " + copyErr.Error())
		return strings.Join(report, "\n"), copyErr
	}
	if closeOutErr != nil {
		add("failed: save upload: " + closeOutErr.Error())
		return strings.Join(report, "\n"), closeOutErr
	}
	if closeInErr != nil {
		add("failed: close upload: " + closeInErr.Error())
		return strings.Join(report, "\n"), closeInErr
	}
	if written == 0 {
		err := errors.New("uploaded file is empty")
		add("failed: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	add("uploaded: " + fileName)

	var newExe string
	lowerName := strings.ToLower(fileName)
	switch {
	case lowerName == "frpc.exe" || strings.HasSuffix(lowerName, ".exe"):
		newExe = uploadPath
	case strings.HasSuffix(lowerName, ".zip"):
		extractDir := filepath.Join(tempDir, "extract")
		if err := unzip(uploadPath, extractDir); err != nil {
			add("failed: extract package: " + err.Error())
			return strings.Join(report, "\n"), err
		}
		add("extract complete")
		found, err := findFile(extractDir, "frpc.exe")
		if err != nil {
			add("failed: frpc.exe not found in package: " + err.Error())
			return strings.Join(report, "\n"), err
		}
		newExe = found
	default:
		err := errors.New("only frpc.exe or frp Windows .zip package is supported")
		add("failed: " + err.Error())
		return strings.Join(report, "\n"), err
	}

	candidateExe, err := makeFrpcUpdateCandidate(tempDir, newExe)
	if err != nil {
		add("failed: prepare frpc.exe: " + err.Error())
		return strings.Join(report, "\n"), err
	}
	defer removeFileWithRetry(candidateExe)
	if err := replaceFrpcExe(p, candidateExe, "", add); err != nil {
		return strings.Join(report, "\n"), err
	}

	cleanupUpdateTempDir(tempDir)
	tempDir = ""
	add("local update finished")
	return strings.Join(report, "\n"), nil
}

func frpcServiceBlocksUpdate() (bool, string) {
	status := strings.TrimSpace(queryServiceStatus(serviceFrpc))
	upper := strings.ToUpper(status)
	if upper == "" || strings.Contains(upper, "STOPPED") || strings.Contains(upper, "NOT INSTALLED") {
		return false, status
	}
	return true, status
}

func stopFrpcBeforeUpdate(add func(string)) (bool, error) {
	status := strings.ToUpper(queryServiceStatus(serviceFrpc))
	if strings.Contains(status, "STOPPED") || strings.Contains(status, "NOT INSTALLED") {
		return false, nil
	}

	wasRunning := strings.Contains(status, "RUNNING") ||
		strings.Contains(status, "START_PENDING") ||
		strings.Contains(status, "STOP_PENDING") ||
		strings.Contains(status, "PAUSED")

	if err := controlService(serviceFrpc, "stop"); err != nil {
		add("failed: stop frpc service: " + err.Error())
		return false, err
	}
	add("frpc service stopped")
	if !wasRunning {
		wasRunning = true
	}
	return wasRunning, nil
}

func startFrpcAfterUpdateIfReady(p paths, add func(string)) error {
	if !configReady(p) {
		add("frpc service not started: config not ready")
		return nil
	}
	if err := controlService(serviceFrpc, "start"); err != nil {
		add("failed: start frpc service: " + err.Error())
		return err
	}
	add("frpc service started")
	return nil
}

func cleanupOldFrpcBackups(p paths) {
	matches, err := filepath.Glob(filepath.Join(p.Base, "frpc.exe.bak.*"))
	if err != nil {
		return
	}
	for _, name := range matches {
		if samePath(name, p.FrpcExe) {
			continue
		}
		_ = removeFileWithRetry(name)
	}
}

func cleanupLegacyDownloadsDir(p paths) {
	legacy := filepath.Join(p.Base, "downloads")
	if samePath(legacy, p.Base) {
		return
	}
	_ = removeAllWithRetry(legacy)
}

func makeUpdateTempDir(kind string) (string, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "update"
	}
	return os.MkdirTemp("", "frpc-web-"+kind+"-")
}

func cleanupUpdateTempDir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	_ = removeAllWithRetry(dir)
}

func removeAllWithRetry(path string) error {
	var lastErr error
	for i := 0; i < 30; i++ {
		_ = chmodTree(path)
		err := os.RemoveAll(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		lastErr = err
		runtime.GC()
		time.Sleep(200 * time.Millisecond)
	}
	return lastErr
}

func removeFileWithRetry(path string) error {
	var lastErr error
	for i := 0; i < 30; i++ {
		_ = os.Chmod(path, 0666)
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		lastErr = err
		runtime.GC()
		time.Sleep(200 * time.Millisecond)
	}
	return lastErr
}

func chmodTree(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0777)
		} else {
			_ = os.Chmod(path, 0666)
		}
		return nil
	})
}

func makeFrpcUpdateCandidate(tempDir string, source string) (string, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "", errors.New("new frpc.exe is empty")
	}
	candidate := filepath.Join(tempDir, "frpc-update-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".exe")
	if err := copyFile(source, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func cleanupFrpcUpdateArtifacts(p paths) {
	cleanupLegacyDownloadsDir(p)
}

func replaceFrpcExe(p paths, newExe string, expectedVersion string, add func(string)) error {
	info, err := os.Stat(newExe)
	if err != nil {
		add("failed: check new frpc.exe: " + err.Error())
		return err
	}
	if info.Size() == 0 {
		err := errors.New("new frpc.exe is empty")
		add("failed: " + err.Error())
		return err
	}

	backupPath := ""
	if frpcExeReady(p.FrpcExe) {
		backupPath = filepath.Join(p.Base, "frpc.exe.bak."+time.Now().Format("20060102150405"))
		if err := copyFile(p.FrpcExe, backupPath); err != nil {
			add("failed: backup old frpc.exe: " + err.Error())
			return err
		}
	}

	if err := copyFile(newExe, p.FrpcExe); err != nil {
		add("failed: replace frpc.exe: " + err.Error())
		if backupPath != "" {
			add("backup kept: " + filepath.Base(backupPath))
		}
		return err
	}

	installedVersion := currentFrpcVersion(p)
	if installedVersion == "" && strings.TrimSpace(expectedVersion) != "" {
		err := errors.New("cannot read replaced frpc.exe version")
		add("failed: verify replacement: " + err.Error())
		if backupPath != "" {
			add("backup kept: " + filepath.Base(backupPath))
		}
		return err
	}
	if installedVersion != "" && expectedVersion != "" && normalizeFrpcVersion(installedVersion) != normalizeFrpcVersion(expectedVersion) {
		err := fmt.Errorf("version mismatch, expected %s got %s", normalizeFrpcVersion(expectedVersion), installedVersion)
		add("failed: verify replacement: " + err.Error())
		if backupPath != "" {
			add("backup kept: " + filepath.Base(backupPath))
		}
		return err
	}
	if installedVersion == "" {
		add("frpc.exe replaced")
	} else {
		add("frpc.exe replaced: " + installedVersion)
	}

	cleanupOldFrpcBackups(p)
	return nil
}

func currentFrpcVersion(p paths) string {
	return frpcVersionOf(p.FrpcExe)
}

func frpcVersionOf(exePath string) string {
	if !frpcExeReady(exePath) {
		return ""
	}
	argsList := [][]string{
		{"-v"},
		{"--version"},
		{"version"},
	}
	for _, args := range argsList {
		out, err := hiddenCommand(exePath, args...).CombinedOutput()
		if err != nil {
			continue
		}
		if v := normalizeFrpcVersion(string(out)); v != "" {
			return v
		}
	}
	return ""
}

func normalizeFrpcVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "frpc")
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) > 0 {
		s = fields[0]
	}
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return ""
	}
	return "v" + s
}

func getFrpRelease(version string) (*githubRelease, error) {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		version = "latest"
	}
	url := "https://api.github.com/repos/fatedier/frp/releases/latest"
	if version != "latest" {
		if !strings.HasPrefix(version, "v") {
			version = "v" + version
		}
		url = "https://api.github.com/repos/fatedier/frp/releases/tags/" + version
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", appName)
	client := &http.Client{Timeout: 40 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("GitHub API status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func windowsArch() string {
	if runtime.GOARCH == "arm64" || strings.Contains(strings.ToUpper(os.Getenv("PROCESSOR_ARCHITECTURE")), "ARM64") {
		return "windows_arm64"
	}
	if runtime.GOARCH == "386" {
		return "windows_386"
	}
	return "windows_amd64"
}

func downloadFile(url, out string) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", appName)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download status: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		clean := filepath.Clean(f.Name)
		if strings.Contains(clean, "..") {
			continue
		}
		path := filepath.Join(dest, clean)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(path)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func findFile(root, name string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), name) {
			found = path
			return io.EOF
		}
		return nil
	})
	if found != "" {
		return found, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return "", fmt.Errorf("%s not found", name)
}

func (w *webApp) runServiceAction(rw http.ResponseWriter, action, elevatedMessage string) {
	if err := runFrpcServiceAction(w.paths, action); err == nil {
		writeJSON(rw, map[string]any{"ok": true, "message": "Done.", "output": statusText(w.paths)})
		return
	}
	if err := runElevated(action, "--base", w.paths.Base); err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": elevatedMessage, "output": elevatedMessage})
}

func (w *webApp) apiAdminRestart(rw http.ResponseWriter, r *http.Request) {
	if isAdmin() {
		writeJSON(rw, map[string]any{"ok": true, "message": "\u5f53\u524d frpc-web \u5df2\u7ecf\u662f\u7ba1\u7406\u5458\u6743\u9650\u3002", "output": statusText(w.paths)})
		return
	}
	if err := launchElevatedWebAfterOldExit(w.paths); err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "\u6b63\u5728\u8bf7\u6c42\u7ba1\u7406\u5458\u6743\u9650\u91cd\u542f frpc-web\u3002"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		logLine(w.paths, "web.log", "stopped: restarting as administrator")
		os.Exit(0)
	}()
}

func launchElevatedWebAfterOldExit(p paths) error {
	return runElevated("admin-web", "--base", p.Base)
}

func (w *webApp) apiLowMemory(rw http.ResponseWriter, r *http.Request) {
	if err := runFrpcServiceAction(w.paths, "start"); err != nil {
		if e := runElevated("start", "--base", w.paths.Base); e != nil {
			writeJSONCode(rw, 500, map[string]any{"ok": false, "message": e.Error()})
			return
		}
	}
	_ = writeSwitch(w.paths.WebOff, true)
	writeJSON(rw, map[string]any{"ok": true, "message": "\u6700\u5c0f\u5185\u5b58\u8fd0\u884c\u5df2\u542f\u7528\uff0cWeb \u540e\u53f0\u5373\u5c06\u5173\u95ed\u3002"})
	go func() {
		time.Sleep(600 * time.Millisecond)
		logLine(w.paths, "web.log", "stopped: low-memory mode")
		os.Exit(0)
	}()
}

func (w *webApp) apiUpdate(rw http.ResponseWriter, r *http.Request) {
	output, err := updateFrpc(w.paths, "latest")
	if err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error(), "output": output})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "output": output})
}

func (w *webApp) apiUpdateUpload(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 256<<20)
	output, err := updateFrpcFromUpload(w.paths, r)
	if err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error(), "output": output})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "output": output})
}

func (w *webApp) apiInstall(rw http.ResponseWriter, r *http.Request) {
	if err := runElevated("install-ui", "--base", w.paths.Base); err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "\u5df2\u8bf7\u6c42\u7ba1\u7406\u5458\u6743\u9650\uff0c\u8bf7\u5728 UAC \u5f39\u7a97\u4e2d\u786e\u8ba4\u5b89\u88c5\u3002"})
}

func (w *webApp) apiUninstall(rw http.ResponseWriter, r *http.Request) {
	if err := runElevated("uninstall", "--base", w.paths.Base); err != nil {
		writeJSONCode(rw, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": "\u5df2\u8bf7\u6c42\u7ba1\u7406\u5458\u6743\u9650\uff0c\u8bf7\u5728 UAC \u5f39\u7a97\u4e2d\u786e\u8ba4\u5378\u8f7d\u3002"})
}

// ---------------- Windows process helpers ----------------

func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// ---------------- Windows service and elevation helpers ----------------

var (
	shell32           = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
	procIsUserAnAdmin = shell32.NewProc("IsUserAnAdmin")
)

func runWindowsService(name string, run func(stop <-chan struct{})) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		_ = err
	}
	if !isService {
		stop := make(chan struct{})
		run(stop)
		return nil
	}
	return svc.Run(name, windowsService{run: run})
}

type windowsService struct {
	run func(stop <-chan struct{})
}

func (s windowsService) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	stop := make(chan struct{})
	done := make(chan struct{})
	status <- svc.Status{State: svc.StartPending}
	go func() {
		s.run(stop)
		close(done)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				close(stop)
				<-done
				return false, 0
			default:
				status <- req.CurrentStatus
			}
		case <-done:
			return false, 0
		}
	}
}

func isAdmin() bool {
	r1, _, _ := procIsUserAnAdmin.Call()
	return r1 != 0
}

func runElevated(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(joinWindowsArgs(args))
	cwd, _ := syscall.UTF16PtrFromString(filepath.Dir(exe))
	r1, _, callErr := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(params)), uintptr(unsafe.Pointer(cwd)), 1)
	if r1 <= 32 {
		if callErr != syscall.Errno(0) {
			return fmt.Errorf("请求管理员权限失败: %v", callErr)
		}
		return fmt.Errorf("请求管理员权限失败，ShellExecute 返回码 %d", r1)
	}
	return nil
}

func joinWindowsArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, quoteWindowsArg(a))
	}
	return strings.Join(parts, " ")
}

func quoteWindowsArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			backslashes++
		case '"':
			b.WriteString(strings.Repeat("\\", backslashes*2+1))
			b.WriteRune(r)
			backslashes = 0
		default:
			if backslashes > 0 {
				b.WriteString(strings.Repeat("\\", backslashes))
				backslashes = 0
			}
			b.WriteRune(r)
		}
	}
	if backslashes > 0 {
		b.WriteString(strings.Repeat("\\", backslashes*2))
	}
	b.WriteByte('"')
	return b.String()
}
