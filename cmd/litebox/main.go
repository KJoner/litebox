// Command litebox 是 LiteBox Panel 的主控服务。
//
// 子命令:
//
//	litebox serve            启动服务(默认)
//	litebox migrate          只执行数据库迁移后退出
//	litebox backup           生成一份 WAL 安全的数据库备份
//	litebox check            数据库完整性与外键自检
//	litebox genkey           生成主密钥
//	litebox ssh-key          打印面板专用的节点访问公钥
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/adjustment"
	"github.com/litebox/litebox/internal/aliyun"
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
	"github.com/litebox/litebox/internal/cloud"
	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/hosttraffic"
	"github.com/litebox/litebox/internal/httpapi"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/notify"
	"github.com/litebox/litebox/internal/portal"
	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/subscription"
	"github.com/litebox/litebox/internal/traffic"
	"github.com/litebox/litebox/internal/user"
	"github.com/litebox/litebox/internal/v2rayapi"
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
	case "backup":
		return cmdBackup(args)
	case "check":
		return cmdCheck(args)
	case "ssh-key":
		return cmdSSHKey(args)
	case "serve":
		return cmdServe(args)
	default:
		return fmt.Errorf("未知命令 %q(可用:serve migrate backup check genkey ssh-key reset-password version)", command)
	}
}

// cmdSSHKey 打印面板专用的节点访问公钥,不存在时生成一把。
//
// 装机脚本用它把公钥展示给管理员:节点接入首选让面板自己用一次性口令去装,
// 但也有些机器只允许密钥登录,那就得先手工把这行贴进节点的 authorized_keys。
func cmdSSHKey(args []string) error {
	fs := flag.NewFlagSet("ssh-key", flag.ExitOnError)
	rotate := fs.Bool("rotate", false, "重新生成密钥(旧公钥在所有节点上立即失效)")
	cfg, _, db, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer db.Close()

	cipher, err := crypto.NewCipher(cfg.Security.MasterKey)
	if err != nil {
		return fmt.Errorf("主密钥校验失败: %w", err)
	}
	mgr := settings.NewKeyManager(settings.NewStore(db, cipher))

	var key settings.PanelKey
	if *rotate {
		key, err = mgr.Rotate(context.Background())
	} else {
		key, err = mgr.Ensure(context.Background())
	}
	if err != nil {
		return err
	}

	fmt.Println(key.PublicKey)
	if *rotate {
		fmt.Fprintln(os.Stderr,
			"\n密钥已轮换。旧公钥在所有节点上立即失效,必须逐个节点重新引导,否则面板连不上它们。")
	}
	return nil
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

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	output := fs.String("output", "", "备份文件路径(默认 <数据目录>/backup/litebox-<时间戳>.db)")
	cfg, logger, db, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer db.Close()

	dest := *output
	if dest == "" {
		// 默认放到数据目录下的 backup 子目录,时间戳精确到秒避免覆盖。
		dest = filepath.Join(cfg.DatabaseDir(), "backup",
			fmt.Sprintf("litebox-%s.db", time.Now().UTC().Format("20060102-150405")))
	}

	size, err := database.Backup(context.Background(), db, dest)
	if err != nil {
		return err
	}
	logger.Info("备份完成", "path", dest, "bytes", size)

	fmt.Printf("\n备份已生成:%s(%.1f KB)\n", dest, float64(size)/1024)
	fmt.Println("\n重要:备份不包含主密钥。")
	fmt.Println("主密钥丢失时,备份中的用户 UUID 与节点私钥全部无法还原,")
	fmt.Println("请把 LITEBOX_MASTER_KEY 与备份文件分开保存,并确认都能取回。")
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	_, _, db, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := database.Check(context.Background(), db)
	if err != nil {
		return err
	}

	fmt.Printf("架构版本    : %d\n", result.SchemaVersion)
	fmt.Printf("日志模式    : %s\n", result.JournalMode)
	fmt.Printf("页数/空闲页 : %d / %d\n", result.PageCount, result.FreelistCount)
	fmt.Printf("完整性检查  : %s\n", passFail(result.IntegrityOK))
	fmt.Printf("外键检查    : %s\n", passFail(result.ForeignKeysOK))
	fmt.Println("\n各表行数:")
	for _, table := range []string{
		"admin_users", "proxy_users", "nodes", "user_nodes",
		"traffic_ledger", "traffic_daily", "deployments", "audit_logs",
	} {
		fmt.Printf("  %-16s %d\n", table, result.TableCounts[table])
	}

	if !result.OK() {
		fmt.Println("\n发现问题:")
		for _, p := range result.Problems {
			fmt.Println("  " + p)
		}
		return fmt.Errorf("数据库自检未通过")
	}
	fmt.Println("\n自检通过")
	return nil
}

func passFail(ok bool) string {
	if ok {
		return "通过"
	}
	return "未通过"
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

	// 面板专用密钥懒生成:首次需要连节点时才建,升级上来的旧库不会平白多出一把。
	settingsStore := settings.NewStore(db, cipher)
	panelKeys := settings.NewKeyManager(settingsStore)

	pool := sshx.NewPool(node.NewResolver(nodeStore, panelKeys, logger), logger)
	defer pool.CloseAll()

	// 流量采集经 SSH 通道读取节点上只监听回环的 V2Ray API。
	sampler := traffic.NewTunnelSampler(pool, func(ctx context.Context, nodeID int64) (string, error) {
		n, err := nodeStore.Get(ctx, nodeID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("127.0.0.1:%d", n.APIPort), nil
	})
	// 共享凭据的入口没有 user 计数器,只能读 inbound 级的那一份。
	// 不接这一条的话,那种入口的流量对面板完全不可见,而节点用量
	// 会静默少算 —— 那正是"0 与真的没用过长得一模一样"的那类失败。
	sampler = sampler.WithSharedInbounds(
		func(ctx context.Context, nodeID int64) ([]v2rayapi.SharedInbound, error) {
			list, err := nodeStore.SharedInboundsForNode(ctx, nodeID)
			if err != nil {
				return nil, err
			}
			out := make([]v2rayapi.SharedInbound, 0, len(list))
			for _, s := range list {
				out = append(out, v2rayapi.SharedInbound{Tag: s.Tag, Code: s.Code})
			}
			return out, nil
		})
	syncer := traffic.NewSyncer(db, sampler, logger)
	// 主机流量(vnStat)那一路(V15)。与代理流量分开:它读的是节点上
	// vnstatd 的数据库,与 sing-box / mita 的计数器完全无关。
	hostTraffic := hosttraffic.NewSyncer(pool, hosttraffic.NewStore(db), logger)

	deployer := deployment.NewDeployer(deployment.Options{
		Pool:   pool,
		Layout: layout,
		// 部署事务的第一步是强制同步流量,失败即中止部署:
		// sing-box 的计数器是纯内存的,重启会让未同步部分永久丢失。
		Syncer:      syncer,
		Logger:      logger,
		KeepBackups: 5,
		// 拨测目标是运行期可改的设置,每次拨测现读。
		ProbeURL: func(ctx context.Context) (string, error) {
			return settingsStore.Get(ctx, settings.KeyProbeURL)
		},
	})
	userStore := user.NewStore(db, cipher)

	// Shadowsocks 密钥的一次性补齐。迁移里做不了 —— 主密钥在 Go 侧,
	// 而这两列存的是密文。跑过一次之后永远是 no-op。
	//
	// 必须在开始服务之前完成:补齐没跑完就把某个节点切成 Shadowsocks 的话,
	// 那一刻起全部存量用户都渲染不进配置,而管理员改的只是一个节点。
	if n, err := nodeStore.BackfillSSKeys(ctx); err != nil {
		return fmt.Errorf("补齐节点 Shadowsocks 密钥: %w", err)
	} else if n > 0 {
		logger.Info("已为存量节点补齐 Shadowsocks 密钥", "节点数", n)
	}
	if n, err := userStore.BackfillSSKeys(ctx); err != nil {
		return fmt.Errorf("补齐用户 Shadowsocks 密钥: %w", err)
	} else if n > 0 {
		logger.Info("已为存量用户补齐 Shadowsocks 密钥", "用户数", n)
	}
	// Mieru 口令同理:缺了它,在某台机器上加一个 Mieru 入口的那一刻起,
	// 全部存量用户都下发不进 mita 的用户列表。
	if n, err := userStore.BackfillMieruPasswords(ctx); err != nil {
		return fmt.Errorf("补齐用户 Mieru 口令: %w", err)
	} else if n > 0 {
		logger.Info("已为存量用户补齐 Mieru 口令", "用户数", n)
	}
	// Snell 凭据同理。这一份缺了还多一层后果:入站的用户列表如果因此
	// 渲染成空,sing-box 会退回单用户模式,而那时 psk 就是唯一凭据 ——
	// psk 在每个人的客户端配置里(见 singbox.ErrSnellNoUsers)。
	if n, err := userStore.BackfillSnellKeys(ctx); err != nil {
		return fmt.Errorf("补齐用户 Snell 凭据: %w", err)
	} else if n > 0 {
		logger.Info("已为存量用户补齐 Snell 凭据", "用户数", n)
	}

	// 外部代理:不属于本面板、不由本面板部署的成品线路。
	// UA 从设置里取,改了不必重启 —— 部分机场按 UA 返回不同格式。
	externalService := externalproxy.NewService(externalproxy.ServiceOptions{
		Store: externalproxy.NewStore(db, cipher),
		UserAgent: func(ctx context.Context) string {
			ua, err := settingsStore.Get(ctx, settings.KeySubscriptionUserAgent)
			if err != nil {
				return ""
			}
			return ua
		},
		Logger: logger,
	})

	relayStore := relay.NewStore(db)

	// mita 与 mieru 客户端都要:前者是服务端,后者只在部署的健康检查里跑
	// 那几秒 —— 而少了它,Mieru 入口的真实拨测就做不了,那是本项目
	// 第一条铁律,Mieru 不给它开口子。
	mieruBinaries := node.NewNamedBinaryProvider(cfg.Node.MieruBinaryDir, "mita",
		"请先执行 scripts/fetch-mieru.sh 拉取")
	mieruClients := node.NewNamedBinaryProvider(cfg.Node.MieruBinaryDir, "mieru",
		"请先执行 scripts/fetch-mieru.sh 拉取")

	nodeService := node.NewService(node.ServiceOptions{
		Store:           nodeStore,
		Pool:            pool,
		Deployer:        deployer,
		DeployStore:     deployment.NewStore(db),
		Users:           userStore,
		Binaries:        node.NewDirBinaryProvider(cfg.Node.BinaryDir),
		PreviewBinaries: node.NewPreviewBinaryProvider(cfg.Node.BinaryDir),
		MieruBinaries:   mieruBinaries,
		MieruClients:    mieruClients,
		RealmBinaries: node.NewNamedBinaryProvider(cfg.Node.RealmBinaryDir, "realm",
			"请先执行 scripts/fetch-realm.sh 拉取"),
		// 引导成功后顺带装 vnStat:失败只记进引导结果,不让创建节点失败。
		HostTraffic:      hostTraffic,
		MieruSync:        syncer,
		Relays:           relayStore,
		RelayHosts:       relayStore,
		Keys:             panelKeys,
		Layout:           layout,
		Logger:           logger,
		BootstrapKeyDirs: cfg.Node.BootstrapKeyDirs,
		SSHDialTimeout:   cfg.Node.SSHDialTimeout,
	})

	// Mieru 的采集器要等 nodeService 建出来才能接上:socket 路径来自
	// deployment.Layout,而只有 node 那一侧知道哪些入口已经下发过。
	// 接在这里而不是塞进 NewSyncer:那个构造函数有好几个调用点,
	// 加一个参数意味着每一处都要改,而其中一处传了 nil 的表现是
	// "这台机器的 Mieru 流量永远是 0" —— 与"真的没人用"长得一模一样。
	syncer.WithMieru(traffic.NewMieruTunnelSampler(pool, nodeService.MieruEndpoints))

	// 用户变更不直接部署,而是标脏后由协调器合并 ——
	// 连续编辑多个用户只会让同一节点重启一次。
	// notifier 先建出来:协调器要在无人值守的部署失败时用它,
	// 而协调器必须在 nodeService.SetTrigger 之前构造。
	notifier := notify.New(settingsStore, logger)
	go notifier.Run(ctx)

	coordinator := deployment.NewCoordinator(deployment.CoordinatorOptions{
		Deployer: nodeService,
		Logger:   logger,
		Debounce: cfg.Node.DeployDebounce,
		MaxDelay: cfg.Node.DeployMaxDelay,
		OnFailure: func(nodeID int64, kind string, result deployment.Result, err error) {
			// 不带 DedupKey:这是由一次具体的变更触发的,
			// 压掉的话管理员改完东西不会知道它没生效。
			notifier.Notify(notify.Event{
				Kind:  notify.KindDeployFailed,
				Level: notify.LevelWarning,
				Title: "自动下发失败",
				Body: fmt.Sprintf(
					`节点 #%d 的%s下发失败。
这次下发是一次变更顺带触发的,没有人在等它。
%v
回滚:%s`,
					nodeID, kind, err, result.RollbackResult),
			})
		},
	})
	// 两者互为依赖,构造期的循环引用在这里显式打断。
	nodeService.SetTrigger(coordinator)
	go coordinator.Run(ctx)

	userService := user.NewService(userStore, coordinator, logger)

	scheduler := traffic.NewScheduler(traffic.SchedulerOptions{
		DB:       db,
		Syncer:   syncer,
		Enforcer: traffic.NewEnforcer(db, logger),
		Trigger:  coordinator,
		Logger:   logger,
		Interval: cfg.Traffic.SyncInterval,
		Host:     hostTraffic,
	})
	go scheduler.Run(ctx)

	// 阿里云 CDT 主机(V17):账号轮询、超阈值停机、定时开关机、保活。
	// 它管的是【实例】,巡检管的是【服务】;停着的实例巡检、采集与流量同步都跳过 ——
	// 一台停着的机器每分钟报一次 connection refused,只会把真正的故障淹掉。
	cloudStore := cloud.NewStore(db, cipher)
	cloudEngine := cloud.New(cloud.Options{
		Store:    cloudStore,
		API:      aliyun.New(),
		Notifier: notifier,
		Logger:   logger,
		Interval: settingsStore.CloudPollInterval,
		Location: settingsStore.CloudLocation,
		NodeName: func(ctx context.Context, id int64) string {
			n, err := nodeStore.Get(ctx, id)
			if err != nil {
				return fmt.Sprintf("节点 #%d", id)
			}
			if n.DisplayName != "" {
				return n.DisplayName
			}
			return n.Name
		},
		NodeHost: func(ctx context.Context, id int64) string {
			n, err := nodeStore.Get(ctx, id)
			if err != nil {
				return ""
			}
			return n.Host
		},
		// 停机之前尽力把这台机器上 sing-box / mita 的计数器同步回来。
		Sync: func(ctx context.Context, id int64) error {
			_, err := scheduler.SyncNodeNow(ctx, id)
			return err
		},
	})
	go cloudEngine.Run(ctx)
	// 阿里云说停着的实例,巡检与资源采集这一轮都跳过;原因进巡检报告。
	cloudSkip := func(ctx context.Context, nodeID int64) string {
		b, err := cloudStore.Binding(ctx, nodeID)
		if err != nil || !b.Stopped() {
			return ""
		}
		reason := "云实例" + b.InstanceStatus.Label()
		if by := b.StoppedBy.Label(); by != "" {
			reason += "(" + by + ")"
		}
		return reason
	}

	// 节点资源监控。间隔为负表示关闭 —— 极端受限的节点上宁可不采。
	metricsStore := node.NewMetricsStore(db)
	var monitor *node.Monitor
	if cfg.Node.MetricsInterval >= 0 {
		monitor = node.NewMonitor(node.MonitorOptions{
			Service:   nodeService,
			Store:     metricsStore,
			Logger:    logger,
			Interval:  cfg.Node.MetricsInterval,
			Retention: cfg.Node.MetricsRetention,
			Skip:      cloudSkip,
		})
		go monitor.Run(ctx)
	} else {
		logger.Info("节点资源监控已关闭(node.metrics_interval 为负)")
	}

	// 节点服务巡检。与资源监控分开:那个可以整个关掉,而这个不能 ——
	// 它回答的是"节点还能不能用",而不是"它忙不忙"。
	watchdog := node.NewWatchdog(node.WatchdogOptions{
		Service:  nodeService,
		Notifier: notifier,
		Logger:   logger,
		Interval: cfg.Node.HealthInterval,
		AutoRecover: func(ctx context.Context) bool {
			return settingsStore.AutoRecoverEnabled(ctx)
		},
		Skip: cloudSkip,
	})
	go watchdog.Run(ctx)

	// 外部代理源的自动同步。每个源自己的间隔决定何时拉,
	// 这里的巡检只是「多久看一眼有没有到点的」。
	// 默认所有源都关着自动同步 —— 打开之前管理员应该先手工同步一次看结果。
	go externalService.Run(ctx)

	// 门户认证与管理员认证是两套独立实现,共用同一个会话时长与限流配置即可。
	portalService := portal.NewService(db, portal.Options{
		SessionTTL:  cfg.Security.SessionTTL,
		MaxAttempts: cfg.Security.LoginMaxAttempts,
		LoginWindow: cfg.Security.LoginWindow,
	})

	profileStore := subscription.NewProfileStore(db)

	server := httpapi.NewServer(httpapi.Options{
		Config:   cfg,
		DB:       db,
		Auth:     authService,
		Audit:    audit.NewRecorder(db, logger),
		Nodes:    nodeService,
		Users:    userService,
		External: externalService,
		Profiles: profileStore,
		Subs: subscription.NewService(
			db, userStore, cipher, cfg.Subscription.ClientMixedPort,
			settingsStore, profileStore, logger),
		Traffic:     traffic.NewQuerier(db),
		Scheduler:   scheduler,
		Metrics:     metricsStore,
		HostTraffic: hostTraffic,
		Monitor:     monitor,
		Watchdog:    watchdog,
		Notifier:    notifier,
		Settings:    settingsStore,
		Tiers:       access.NewStore(db),
		Cloud:       cloudEngine,
		CloudStore:  cloudStore,

		Portal:      portalService,
		PortalAccts: portal.NewStore(db),
		Relays:      relayStore,
		PortalData:  portal.NewQuerier(db, userStore),
		Adjustments: adjustment.NewStore(db),

		Pool:     pool,
		Binaries: node.NewDirBinaryProvider(cfg.Node.BinaryDir),
		Logger:   logger,
		Assets:   assets,
	})

	go cleanupLoop(ctx, authService, portalService, logger)

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
// 管理员与门户是两套表,两边都要清 —— 只清一边会让另一边无节制增长。
func cleanupLoop(ctx context.Context, authService *auth.Service,
	portalService *portal.Service, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := authService.CleanupExpired(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("清理过期管理员会话失败", "error", err)
		}
		if err := portalService.CleanupExpired(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("清理过期门户会话失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
