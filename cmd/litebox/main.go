// Command litebox 是 LiteBox Panel 的主控服务。
//
// 子命令:
//
//	litebox serve            启动服务(默认)
//	litebox migrate          只执行数据库迁移后退出
//	litebox genkey           生成主密钥
//	litebox reset-password   重置管理员密码
//	litebox version          打印版本
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/httpapi"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/subscription"
	"github.com/litebox/litebox/internal/traffic"
	"github.com/litebox/litebox/internal/user"
	"github.com/litebox/litebox/web"
)

// Version 由构建时的 -ldflags 注入。
var Version = "dev"

const defaultAdminUsername = "admin"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "genkey":
		return cmdGenKey()
	case "version":
		fmt.Printf("LiteBox %s\n", Version)
		return nil
	case "migrate":
		return cmdMigrate(args)
	case "reset-password":
		return cmdResetPassword(args)
	case "serve":
		return cmdServe(args)
	default:
		return fmt.Errorf("未知命令 %q(可用:serve migrate genkey reset-password version)", command)
	}
}

func cmdGenKey() error {
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		return err
	}
	fmt.Println(key)
	fmt.Fprintln(os.Stderr, "\n请将其保存为环境变量 LITEBOX_MASTER_KEY。")
	fmt.Fprintln(os.Stderr, "主密钥用于加密用户 UUID 与节点私钥,丢失后这些数据将无法还原。")
	return nil
}

// setup 完成配置加载、日志、数据库连接与迁移,是各子命令的共同前置。
func setup(fs *flag.FlagSet, args []string) (config.Config, *slog.Logger, *sql.DB, error) {
	configPath := fs.String("config", "litebox.yaml", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, nil, nil, err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return cfg, nil, nil, err
	}

	logger := newLogger(cfg.Log)

	db, err := database.Open(cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		return cfg, logger, nil, err
	}
	if err := database.Migrate(db, logger); err != nil {
		db.Close()
		return cfg, logger, nil, err
	}
	return cfg, logger, db, nil
}

func newLogger(cfg config.LogConfig) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(cfg.Format) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	_, logger, db, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := database.CheckIntegrity(db); err != nil {
		return err
	}
	logger.Info("迁移完成,数据库一致性检查通过")
	return nil
}

func cmdResetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	username := fs.String("username", defaultAdminUsername, "要重置的管理员用户名")
	cfg, logger, db, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer db.Close()

	password, err := crypto.GenerateToken(12)
	if err != nil {
		return err
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE admin_users SET password_hash=?, updated_at=? WHERE username=?`,
		hash, now, *username)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("管理员 %q 不存在", *username)
	}
	// 重置密码后所有会话必须失效,否则旧 Cookie 仍然可用。
	if _, err := db.Exec(
		`DELETE FROM admin_sessions WHERE admin_user_id = (SELECT id FROM admin_users WHERE username=?)`,
		*username); err != nil {
		return err
	}
	_ = cfg
	logger.Info("密码已重置,所有会话已失效", "username", *username)
	fmt.Printf("\n新密码:%s\n\n", password)
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, logger, db, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer db.Close()

	// 主密钥必须在启动时就验证可用,不能等到第一次加密用户 UUID 时才失败。
	cipher, err := crypto.NewCipher(cfg.Security.MasterKey)
	if err != nil {
		return fmt.Errorf("主密钥校验失败: %w", err)
	}

	authService := auth.NewService(db, auth.Options{
		SessionTTL:   cfg.Security.SessionTTL,
		MaxAttempts:  cfg.Security.LoginMaxAttempts,
		LoginWindow:  cfg.Security.LoginWindow,
		LoginLockout: cfg.Security.LoginLockout,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	created, password, err := authService.EnsureAdmin(ctx, defaultAdminUsername)
	if err != nil {
		return fmt.Errorf("初始化管理员账号: %w", err)
	}
	if created {
		logger.Warn("已创建初始管理员账号,请登录后立即修改密码",
			"username", defaultAdminUsername)
		fmt.Printf("\n初始管理员账号:%s\n初始密码:%s\n\n", defaultAdminUsername, password)
	}

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("加载前端资源: %w", err)
	}

	// 节点能力:存储 → 连接池 → 部署器 → 服务。
	nodeStore := node.NewStore(db, cipher)
	layout := deployment.DefaultLayout()

	pool := sshx.NewPool(node.NewResolver(nodeStore, logger), logger)
	defer pool.CloseAll()

	// 流量采集经 SSH 通道读取节点上只监听回环的 V2Ray API。
	sampler := traffic.NewTunnelSampler(pool, func(ctx context.Context, nodeID int64) (string, error) {
		n, err := nodeStore.Get(ctx, nodeID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("127.0.0.1:%d", n.APIPort), nil
	})
	syncer := traffic.NewSyncer(db, sampler, logger)

	deployer := deployment.NewDeployer(deployment.Options{
		Pool:   pool,
		Layout: layout,
		// 部署事务的第一步是强制同步流量,失败即中止部署:
		// sing-box 的计数器是纯内存的,重启会让未同步部分永久丢失。
		Syncer:      syncer,
		Logger:      logger,
		KeepBackups: 5,
	})
	userStore := user.NewStore(db, cipher)
	nodeService := node.NewService(node.ServiceOptions{
		Store:       nodeStore,
		Pool:        pool,
		Deployer:    deployer,
		DeployStore: deployment.NewStore(db),
		Users:       userStore,
		Layout:      layout,
		Logger:      logger,
	})

	// 用户变更不直接部署,而是标脏后由协调器合并 ——
	// 连续编辑多个用户只会让同一节点重启一次。
	coordinator := deployment.NewCoordinator(deployment.CoordinatorOptions{
		Deployer: nodeService,
		Logger:   logger,
		Debounce: cfg.Node.DeployDebounce,
		MaxDelay: cfg.Node.DeployMaxDelay,
	})
	go coordinator.Run(ctx)

	userService := user.NewService(userStore, coordinator, logger)

	scheduler := traffic.NewScheduler(traffic.SchedulerOptions{
		DB:       db,
		Syncer:   syncer,
		Enforcer: traffic.NewEnforcer(db, logger),
		Trigger:  coordinator,
		Logger:   logger,
		Interval: cfg.Traffic.SyncInterval,
	})
	go scheduler.Run(ctx)

	server := httpapi.NewServer(httpapi.Options{
		Config:    cfg,
		DB:        db,
		Auth:      authService,
		Audit:     audit.NewRecorder(db, logger),
		Nodes:     nodeService,
		Users:     userService,
		Subs:      subscription.NewService(db, userStore, cipher, cfg.Subscription.ClientMixedPort),
		Traffic:   traffic.NewQuerier(db),
		Scheduler: scheduler,
		Pool:      pool,
		Binaries:  node.NewDirBinaryProvider(cfg.Node.BinaryDir),
		Logger:    logger,
		Assets:    assets,
	})

	go cleanupLoop(ctx, authService, logger)

	errCh := make(chan error, 1)
	go func() { errCh <- server.Start() }()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("收到关闭信号,正在优雅关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("关闭 HTTP 服务: %w", err)
	}
	// 待部署的节点必须冲刷完再退出,否则数据库状态与节点实际配置会长期不一致。
	coordinator.Wait(cfg.HTTP.ShutdownTimeout)
	logger.Info("已关闭")
	return nil
}

// cleanupLoop 定期清理过期会话与陈旧的登录尝试记录。
func cleanupLoop(ctx context.Context, authService *auth.Service, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := authService.CleanupExpired(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("清理过期会话失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
