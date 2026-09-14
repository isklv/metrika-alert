// Command server runs the Metrika alert engine: it polls Yandex.Metrika
// counters, evaluates triggers against the events, and delivers alerts and
// periodic reports to Telegram, VK Teams and webhooks.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/alert"
	"github.com/isklv/metrika-alert/internal/api"
	"github.com/isklv/metrika-alert/internal/bot"
	"github.com/isklv/metrika-alert/internal/config"
	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
	"github.com/isklv/metrika-alert/internal/vkteams"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	// -health is the container health probe: it runs inside the same image,
	// so no HTTP client needs to be installed in the runtime layer.
	if len(os.Args) > 1 && os.Args[1] == "-health" {
		os.Exit(runHealthCheck())
	}

	cfgPath := findConfig()
	if cfgPath == "" {
		log.Fatalf("no config found: create %s (copy config.example.yaml)",
			filepath.Join(homeDir(), ".metrika-alert", "config.yaml"))
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("config loaded from %s", cfgPath)

	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Chat clients -------------------------------------------------------
	// Built before the router so that alert delivery and command handling share
	// one connection per platform, including its proxy settings.
	//
	// A channel that will not connect is disabled rather than fatal. The two
	// platforms fail independently — Telegram is commonly unreachable from
	// Russian networks without a proxy — and letting one take the process down
	// would silence the other along with it.
	tgBot, err := newTelegramBot(cfg)
	if err != nil {
		log.Printf("WARNING: telegram disabled — %v", err)
		tgBot = nil
	}
	vkClient, err := newVKTeamsClient(cfg)
	if err != nil {
		log.Printf("WARNING: vkteams disabled — %v", err)
		vkClient = nil
	}

	routerOpts := alert.Options{TelegramAdmins: cfg.Telegram.AdminIDs, VKTeamsAdmins: cfg.VKTeams.AdminIDs}
	if tgBot != nil {
		routerOpts.Telegram = tgBot
	}
	if vkClient != nil {
		routerOpts.VKTeams = vkClient
	}
	router := alert.NewRouter(db, routerOpts)
	log.Printf("alert channels: %s", strings.Join(router.Channels(), ", "))

	if tgBot == nil && vkClient == nil {
		log.Printf("WARNING: ни один чат-канал не поднялся — алерты дойдут только до webhook-адресатов")
	}

	// --- Engine -------------------------------------------------------------
	metrikaCfg := &engine.MetrikaConfig{
		BaseURL:         cfg.Metrika.BaseURL,
		SettleMinutes:   cfg.Metrika.SettleMinutes,
		WindowMinutes:   cfg.Metrika.WindowMinutes,
		StepMinutes:     cfg.Metrika.StepMinutes,
		MaxCatchUpSteps: cfg.Metrika.MaxCatchUpSteps,
	}
	evaluator := engine.NewEvaluator(db, router, metrikaCfg)
	poller := engine.NewPoller(db, metrikaCfg, evaluator)
	reporter := engine.NewReporter(db, metrikaCfg, router)

	if err := poller.Start(ctx); err != nil {
		log.Fatalf("poller: %v", err)
	}

	var wg sync.WaitGroup
	tasks := &engineTasks{poller: poller, reporter: reporter}

	if cfg.ReportHour > 0 {
		wg.Go(func() {
			runReportsLoop(ctx, reporter, time.Duration(cfg.ReportHour)*time.Hour)
		})
	} else {
		log.Printf("periodic reports disabled (report_interval_hours = 0)")
	}

	// --- Bots ---------------------------------------------------------------
	if tgBot != nil {
		admins := make([]string, 0, len(cfg.Telegram.AdminIDs))
		for _, id := range cfg.Telegram.AdminIDs {
			admins = append(admins, strconv.FormatInt(id, 10))
		}
		warnIfNoAdmins("telegram", len(admins))

		b := bot.New(bot.NewTelegramTransport(tgBot), db, tasks, admins)
		wg.Go(func() { bot.RunTelegram(ctx, tgBot, b) })
	}

	if vkClient != nil {
		warnIfNoAdmins("vkteams", len(cfg.VKTeams.AdminIDs))

		b := bot.New(bot.NewVKTeamsTransport(vkClient), db, tasks, cfg.VKTeams.AdminIDs)
		wg.Go(func() { bot.RunVKTeams(ctx, vkClient, b) })
	}

	// --- REST API -----------------------------------------------------------
	if cfg.API.Enabled {
		apiServer := api.NewServer(db, reporter, poller, api.Config{
			ListenAddr: cfg.API.ListenAddr,
			AuthToken:  cfg.API.AuthToken,
			Metrika:    metrikaCfg,
		})
		wg.Go(func() {
			if err := apiServer.ListenAndServe(ctx); err != nil {
				log.Printf("api server: %v", err)
			}
		})
	}

	log.Printf("metrika-alert running — press Ctrl+C to stop")
	<-ctx.Done()
	stop() // restore default signal handling: a second Ctrl+C aborts immediately

	log.Println("shutting down…")
	waitWithTimeout(&wg, 10*time.Second)
	log.Println("stopped")
}

// engineTasks lets the chat bots trigger the background jobs on demand.
type engineTasks struct {
	poller   *engine.Poller
	reporter *engine.Reporter
}

// RunReport builds and delivers a report; counterID 0 means every counter.
func (t *engineTasks) RunReport(ctx context.Context, counterID int64) error {
	if counterID <= 0 {
		return t.reporter.RunReports(ctx)
	}
	return t.reporter.RunReportFor(ctx, counterID)
}

func (t *engineTasks) PollOnce(ctx context.Context) error {
	return t.poller.RunOnce(ctx)
}

// newTelegramBot returns nil when no token is configured.
func newTelegramBot(cfg *config.Config) (*tgbotapi.BotAPI, error) {
	if cfg.Telegram.BotToken == "" {
		return nil, nil
	}

	transport, err := proxyTransport("telegram", cfg.Telegram.ProxyURL)
	if err != nil {
		return nil, err
	}
	client := http.DefaultClient
	if transport != nil {
		client = &http.Client{Transport: transport}
	}

	api, err := tgbotapi.NewBotAPIWithClient(cfg.Telegram.BotToken, tgbotapi.APIEndpoint, client)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к api.telegram.org: %w%s", err, telegramHint(cfg))
	}
	log.Printf("telegram bot ready: @%s", api.Self.UserName)
	return api, nil
}

// newVKTeamsClient returns nil when no token is configured.
func newVKTeamsClient(cfg *config.Config) (*vkteams.Client, error) {
	if cfg.VKTeams.BotToken == "" {
		return nil, nil
	}

	transport, err := proxyTransport("vkteams", cfg.VKTeams.ProxyURL)
	if err != nil {
		return nil, err
	}
	client, err := vkteams.New(cfg.VKTeams.BotToken, vkteams.Options{
		BaseURL:   cfg.VKTeams.BaseURL,
		Transport: transport,
	})
	if err != nil {
		return nil, err
	}

	// Fail fast on a bad token or an unreachable on-premise endpoint, rather
	// than discovering it when the first alert needs to go out.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	self, err := client.GetSelf(ctx)
	if err != nil {
		return nil, fmt.Errorf("не удалось проверить токен на %s: %w", client.BaseURL(), err)
	}
	log.Printf("vkteams bot ready: %s (%s) at %s", self.Nick, self.UserID, client.BaseURL())
	return client, nil
}

// proxySchemes are what net/http's Transport can actually route through.
// socks5h is a common and silent mistake: curl accepts it, Go does not.
var proxySchemes = map[string]bool{"http": true, "https": true, "socks5": true}

// proxyTransport builds the transport a chat client should use, or nil for a
// direct connection.
//
// The scheme is checked rather than trusted. url.Parse accepts
// "proxy.example:1080" by reading the host as the scheme, which would leave the
// client connecting directly while the config plainly names a proxy — the exact
// failure this reports instead.
func proxyTransport(platform, proxyURL string) (http.RoundTripper, error) {
	if proxyURL == "" {
		log.Printf("%s: прямое подключение, прокси не задан", platform)
		return nil, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy_url %q не разобрался: %w", proxyURL, err)
	}
	if !proxySchemes[parsed.Scheme] || parsed.Host == "" {
		return nil, fmt.Errorf(
			"proxy_url %q: нужна схема http://, https:// или socks5:// и хост (socks5h Go не поддерживает)",
			proxyURL)
	}

	log.Printf("%s: через прокси %s", platform, parsed.Redacted())
	return &http.Transport{Proxy: http.ProxyURL(parsed)}, nil
}

// telegramHint points at the usual cause. api.telegram.org resolves into
// 149.154.160.0/20, so a timeout on an address in that range is a blocked or
// unrouted path to Telegram — not a problem with the token or with VK Teams.
func telegramHint(cfg *config.Config) string {
	if cfg.Telegram.ProxyURL == "" {
		return " (сеть до Telegram недоступна? задайте telegram.proxy_url)"
	}
	// Never echo the raw value: a proxy URL routinely carries a password.
	return " (через прокси " + redactProxy(cfg.Telegram.ProxyURL) + " — проверьте, что он жив)"
}

// redactProxy strips credentials from a proxy URL for display.
func redactProxy(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(указанный прокси)"
	}
	return parsed.Redacted()
}

func warnIfNoAdmins(platform string, count int) {
	if count == 0 {
		log.Printf("WARNING: %s.admin_ids is empty — the bot will refuse every command. "+
			"Message it and it will reply with the ID to add.", platform)
	}
}

func runReportsLoop(ctx context.Context, reporter *engine.Reporter, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("periodic reports every %s", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reporter.RunReports(ctx); err != nil {
				log.Printf("report run: %v", err)
			}
		}
	}
}

// waitWithTimeout bounds shutdown: a bot stuck in a long poll must not hold the
// process open past the container's stop grace period.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("shutdown timed out after %s, exiting anyway", timeout)
	}
}

// runHealthCheck probes the local API and returns a process exit code.
// With the API disabled there is nothing to probe, and the fact that this
// binary started at all is the only signal available — so it reports healthy.
func runHealthCheck() int {
	cfgPath := findConfig()
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "health: no config found")
		return 1
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: %v\n", err)
		return 1
	}
	if !cfg.API.Enabled {
		return 0
	}

	addr := cfg.API.ListenAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health: /healthz returned %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// findConfig locates the config file: an explicit path argument first, then
// ~/.metrika-alert/config.yaml, then the working directory. Flags are skipped
// so that "-health /etc/metrika-alert/config.yaml" resolves the same path the
// running service uses.
func findConfig() string {
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	if home := homeDir(); home != "" {
		path := filepath.Join(home, ".metrika-alert", "config.yaml")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return ""
}

func openDB(cfg *config.Config) (*model.DB, error) {
	path := cfg.Database
	if !filepath.IsAbs(path) && homeDir() != "" {
		path = filepath.Join(homeDir(), ".metrika-alert", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	return model.OpenDB(path)
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE")
}
