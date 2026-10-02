// main.go trae2api-web 入口：加载配置 → 构建 pool → 起 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/scheduler"
	"trae2api-web/internal/server"
	"trae2api-web/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// 存量迁移：hex32/UUID 设备号 → 本账号独立十进制号（claim 9074 根因修复，
	// 见 upstream/deviceid.go；已是十进制的号不动）。
	for _, a := range auths {
		if upstream.IsDecimalDeviceID(a.DeviceID) {
			continue
		}
		did, derr := upstream.NewDeviceID()
		if derr != nil {
			log.Printf("device id migrate uid=%s: %v", a.UID, derr)
			continue
		}
		old := a.DeviceID
		a.DeviceID = did
		if err := a.SaveAtomic(); err != nil {
			log.Printf("device id migrate save uid=%s: %v", a.UID, err)
			a.DeviceID = old
			continue
		}
		log.Printf("device id migrate uid=%s: %s → %s", a.UID, old, did)
	}

	p := pool.New(cfg.StateFile)
	p.SyncToDir(auths) // 对齐：剔除 state.json 中已删除 auth 文件的幽灵账号

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 流式客户端无总超时，仅用首字节兜底（时长由 SSE 流本身决定）。
	if tr, ok := up.StreamHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	}

	checkinHours := cfg.Schedule.CheckinHours
	if len(checkinHours) == 0 {
		checkinHours = []int{cfg.Schedule.CheckinHour}
	}
	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   checkinHours,
		RefreshHours:   cfg.Schedule.RefreshHours,
		JitterMinutes:  cfg.Schedule.JitterMinutes,
		BalanceRefresh: time.Duration(cfg.Schedule.BalanceRefreshMin) * time.Minute,
		RefreshSkew:    24 * time.Hour,

		// 签到日志（面板「自动签到」看板）与结果推送。
		LogPath:    filepath.Join(filepath.Dir(cfg.StateFile), "checkin.jsonl"),
		Notifier:   scheduler.NewNotifier(cfg.CheckinWebhook),
		NotifyMode: cfg.CheckinNotify,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		AuthDir:      cfg.AuthDir,
		DataDir:      filepath.Dir(cfg.StateFile),
		Prompt:       server.PromptConfig{Mode: cfg.Prompt.Mode, File: cfg.Prompt.File},
		PlanCooldown: cfg.PlanCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		DefaultModel: cfg.DefaultModel,
		Sched:        sch, // /admin 手动签到/刷新按钮的执行体

		CallbackBase: cfg.CallbackBase, // 空 = 回调仍用 127.0.0.1:<port>

		AdminPassword: cfg.AdminPassword, // 非空 = 面板启用内置登录页

		LogRetentionDays: cfg.LogRetentionDays, // 日志保留天数（0 = 关闭自动清理）
	})

	// 启动后异步自检代理池：出口 IP / 延迟 / 状态写回 data/proxies.json，面板直接可见。
	// 只探测，不影响任何账号状态。
	go h.ProbeAllProxies()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	// 日志保留：启动清理一次 + 每 24h 一次（usage.jsonl / checkin.jsonl）。
	h.StartLogRetention(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// 第二个 http.Server：监听 CallbackPort（默认 18080），只处理 /authorize 回调。
	// 复用同一 Handler（/authorize 已在主 mux 注册）。
	// cfg.CallbackPort == "0" 时不启动（纯手动粘贴模式）。
	var cbSrv *http.Server
	if cfg.CallbackPort != "" && cfg.CallbackPort != "0" {
		cbSrv = &http.Server{
			Addr:              "127.0.0.1:" + cfg.CallbackPort,
			Handler:           h,
			ReadHeaderTimeout: 30 * time.Second,
		}
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = cbSrv.Shutdown(sc)
		}()
		go func() {
			log.Printf("trae2api-web callback server on 127.0.0.1:%s (TRAE login /authorize)", cfg.CallbackPort)
			if err := cbSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// 端口被占用（login.sh / 旧实例）不致命，降级为手动粘贴模式。
				log.Printf("callback server (:%s) failed: %v — web 登录降级为手动粘贴回调链接", cfg.CallbackPort, err)
			}
		}()
	}

	log.Printf("trae2api-web listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
