package main

import (
	"campaignstudio/internal/studio"
	"context"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	role := env("ROLE", "api")
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	store, err := studio.OpenStore(ctx, env("DATABASE_URL", "postgres://studio:studio@localhost:5432/studio?sslmode=disable"))
	if err != nil {
		slog.Error("database", "error", err)
		os.Exit(1)
	}
	defer store.DB.Close()
	if role == "migrate" {
		if err = store.Migrate(ctx); err != nil {
			slog.Error("migration", "error", err)
			os.Exit(1)
		}
		slog.Info("schema ready; 5000 synthetic users seeded")
		return
	}
	opt := asynq.RedisClientOpt{Addr: env("REDIS_ADDR", "localhost:6379"), ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
	rd := redis.NewClient(&redis.Options{Addr: opt.Addr})
	defer rd.Close()
	queue := asynq.NewClient(opt)
	defer queue.Close()
	provider := &studio.HTTPProvider{URL: env("PROVIDER_URL", "http://localhost:8090"), Client: &http.Client{Timeout: time.Second}}
	registry := &studio.Registry{Channels: map[string]studio.Channel{"sms": studio.SMSChannel{}, "email": studio.EmailChannel{}}, Providers: map[string]studio.Provider{"sms": provider, "email": provider}}
	var llm studio.LLM = studio.MockLLM{}
	mode := env("LLM_MODE", "mock")
	if mode == "claude" {
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			slog.Error("ANTHROPIC_API_KEY required for LLM_MODE=claude")
			os.Exit(1)
		}
		llm = &studio.ClaudeLLM{Key: key, Model: env("ANTHROPIC_MODEL", "claude-sonnet-4-5"), Client: &http.Client{Timeout: 80 * time.Second}}
	} else if mode != "mock" {
		slog.Error("LLM_MODE must be mock or claude")
		os.Exit(1)
	}
	ttl, err := time.ParseDuration(env("CAMPAIGN_TTL", "0s"))
	if err != nil || ttl < 0 {
		slog.Error("CAMPAIGN_TTL must be a nonnegative Go duration")
		os.Exit(1)
	}
	engine := &studio.Engine{Store: store, Redis: rd, Queue: queue, Registry: registry, LLM: llm, CampaignTTL: ttl}
	if role == "dispatcher" {
		engine.RunDispatcher(ctx)
		return
	}
	if role == "worker" {
		concurrency, _ := strconv.Atoi(env("WORKER_CONCURRENCY", "32"))
		if concurrency < 1 {
			concurrency = 32
		}
		if err = engine.RunWorkers(ctx, opt, env("WORKER_QUEUE", "sms"), concurrency); err != nil {
			slog.Error("worker", "error", err)
			os.Exit(1)
		}
		return
	}
	var handler http.Handler
	addr := ":8081"
	if role == "provider" {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /send", store.ProviderHandler)
		mux.HandleFunc("GET /receipts/{key}", store.ProviderHandler)
		mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		handler = mux
		addr = ":8090"
	} else {
		handler = engine.Handler(mode)
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		server.Shutdown(shutdown)
	}()
	slog.Info("listening", "role", role, "addr", addr)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http", "error", err)
		os.Exit(1)
	}
}
