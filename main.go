// wecom-bot daemon：维持企业微信机器人连接，按群聊路由到独立 session
// （每个群一个 nekocode --headless 子进程）。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wecom-bot/router"
	"wecom-bot/session"
	"wecom-bot/wecom"
)

type config struct {
	botID       string
	secret      string
	bin         string
	workDirBase string
	sharedDir   string
}

func loadConfig() (config, error) {
	var cfg config
	flag.StringVar(&cfg.botID, "bot-id", os.Getenv("WECOM_BOT_ID"), "企业微信机器人 BotID（或 WECOM_BOT_ID）")
	flag.StringVar(&cfg.secret, "secret", os.Getenv("WECOM_BOT_SECRET"), "企业微信机器人 Secret（或 WECOM_BOT_SECRET）")
	flag.StringVar(&cfg.bin, "bin", envOr("NEKOCODE_BIN", "nekocode-tui"), "nekocode CLI 可执行文件")
	flag.StringVar(&cfg.workDirBase, "workdir", os.Getenv("WECOM_WORKDIR"), "每群独立工作目录的父目录；不设置则用进程当前目录")
	flag.StringVar(&cfg.sharedDir, "shared-workdir", os.Getenv("WECOM_SHARED_WORKDIR"), "所有群共用的已存在工作区（设置后忽略 -workdir 的按群划分）")
	flag.Parse()
	if cfg.botID == "" || cfg.secret == "" {
		return cfg, errors.New("缺少凭证：请设置 -bot-id/-secret 或 WECOM_BOT_ID/WECOM_BOT_SECRET")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[wecom-bot] ")
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := wecom.New(cfg.botID, cfg.secret)
	rt := router.New(router.Options{
		Session: session.Options{
			Bin: cfg.bin,
		},
		WorkDirBase:   cfg.workDirBase,
		SharedWorkDir: cfg.sharedDir,
	}, client)
	defer rt.Close()

	// WS 重连循环：认证后接收消息并路由；被踢下线则放弃。
	go func() {
		backoff := time.Second
		for {
			err := client.Serve(ctx,
				func() { backoff = time.Second; log.Printf("已连接企业微信") },
				func(msgCtx context.Context, reqID string, msg wecom.Message) {
					go rt.Dispatch(msgCtx, client, reqID, msg)
				})
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, wecom.ErrSuperseded) {
				log.Fatalf("连接被取代：同一 BotID 存在另一个客户端连接")
			}
			log.Printf("连接断开: %v，%v 后重连", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}()

	<-ctx.Done()
	log.Printf("正在退出，活跃会话: %d", rt.Count())
	// rt.Close 由 defer 执行，给子进程 2s 清理时间
	time.Sleep(2 * time.Second)
}
