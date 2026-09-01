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
	"strings"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/alert"
	"github.com/isklv/metrika-alert/internal/api"
	"github.com/isklv/metrika-alert/internal/bot"
	"github.com/isklv/metrika-alert/internal/config"
	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

func main() {
	cfgPath := findConfig()
	if cfgPath == "" {
		log.Fatalf("no config found: create %s (copy config.example.yaml)", filepath.Join(homeDir(), ".metrika-alert", "config.yaml"))
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	router, err := newAlertRouter(cfg.Telegram.BotToken, db)
	if err != nil {
		log.Fatalf("alert router: %v", err)
	}

	eval := engine.NewEvaluator(db, router)
	metrikaCfg := &engine.MetrikaConfig{BaseURL: cfg.Metrika.BaseURL, LogsURL: cfg.Metrika.LogsURL}
	poller := engine.NewPoller(db, metrikaCfg, func(counterID int64, events []*model.MetrikaEvent) error {
		if err := eval.Evaluate(context.Background(), counterID, events); err != nil {
			log.Printf("evaluate: %v", err)
		}
		return nil
	})
	reporter := engine.NewReporter(db, metrikaCfg, router)

	// Start background workers.
	ctx, cancel := signalContext()
	defer cancel()

	if err := poller.Start(ctx); err != nil && err != context.Canceled {
		log.Fatalf("poller: %v", err)
	}

	go runReportsLoop(ctx, reporter, time.Duration(cfg.ReportHour)*time.Hour)

	// Telegram bot.
	var botActions *bot.BotActions
	if cfg.Telegram.BotToken != "" {
		tgClient := http.DefaultClient
		if cfg.Telegram.ProxyURL != "" {
			proxy, err := url.Parse(cfg.Telegram.ProxyURL)
			if err != nil {
				log.Fatalf("parse proxy_url: %v", err)
			}
			tgClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
			log.Printf("telegram using proxy: %s", cfg.Telegram.ProxyURL)
		}

		tgBot, err := newBotAPI(cfg.Telegram.BotToken, tgClient)
		if err != nil {
			log.Fatalf("telegram bot: %v", err)
		}
		b := bot.NewBot(tgBot, db, cfg.Telegram.AdminIDs)
		botActions = bot.NewBotActions(b)
		b.SetActions(botActions)

		u := tgbotapi.NewUpdate(0)
		u.Timeout = 60
		ch := tgBot.GetUpdatesChan(u)

		go handleUpdates(ch, botActions)
		log.Printf("telegram bot running: @%s", tgBot.Self.UserName)
	}

	// REST API.
	if cfg.API.Enabled {
		apiServer := api.NewServer(db, reporter, poller, cfg.API.ListenAddr)
		go func() {
			if err := apiServer.ListenAndServe(ctx); err != nil {
				log.Printf("api server: %v", err)
			}
		}()
	}

	log.Printf("metrika-alert running — press Ctrl+C to stop")
	<-ctx.Done()
	log.Println("shutting down…")
	time.Sleep(2 * time.Second)
}

// newBotAPI creates the Telegram bot API client. A package-level variable so
// tests can inject a fake endpoint instead of hitting api.telegram.org.
var newBotAPI = func(token string, client *http.Client) (*tgbotapi.BotAPI, error) {
	return tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
}

// newAlertRouter creates the alert delivery router. A package-level variable
// so tests can avoid the real getMe call made by alert.NewRouter for a
// non-empty token.
var newAlertRouter = func(token string, db *model.DB) (*alert.Router, error) {
	return alert.NewRouter(token, db)
}

// reportRunner is the part of *engine.Reporter used by runReportsLoop.
type reportRunner interface {
	RunReports(ctx context.Context) error
}

func runReportsLoop(ctx context.Context, reporter reportRunner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

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

func handleUpdates(ch tgbotapi.UpdatesChannel, actions *bot.BotActions) {
	for update := range ch {
		if update.Message == nil || !update.Message.IsCommand() && update.Message.Text == "" {
			continue
		}
		actions.ProcessMessage(*update.Message)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()
	return ctx, cancel
}

func findConfig() string {
	// Check: explicit arg, ~/.metrika-alert/config.yaml, current dir.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		return os.Args[1]
	}
	home := homeDir()
	if home != "" {
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
