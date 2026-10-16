package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ghc2api/internal/copilot"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		logger.Error("程序退出", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (runErr error) {
	configPath, err := configPath(args)
	if err != nil {
		return err
	}
	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	logger, logFile, err := newConfiguredLogger(config, stderr)
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("关闭日志文件失败: %w", err)
		}
	}()
	logger.InfoContext(ctx, "配置加载完成", "config_file", configPath, "log_level", config.LogLevel)
	if config.insecurePermissions {
		logger.WarnContext(ctx, "配置文件权限过宽，建议执行 chmod 600", "config_file", configPath,
			"mode", fmt.Sprintf("%04o", config.configMode))
	}
	for _, account := range config.Accounts {
		if account.insecureCredential {
			logger.WarnContext(ctx, "凭证文件权限过宽，建议执行 chmod 600", "credential_file", account.credentialFile,
				"mode", fmt.Sprintf("%04o", account.credentialMode))
		}
	}
	return serve(ctx, config, logger)
}

func configPath(args []string) (string, error) {
	switch {
	case len(args) == 0:
		return "config.yaml", nil
	case len(args) == 2 && args[0] == "-c" && strings.TrimSpace(args[1]) != "":
		return args[1], nil
	default:
		return "", errors.New("启动参数仅支持 -c <配置文件>；未指定时默认读取 config.yaml")
	}
}

func serve(ctx context.Context, config appConfig, logger *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pool, err := copilot.NewManagedPool(config.poolConfig(logger), config.poolAccounts())
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("无法监听 %s: %w", config.Listen, err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler: pool.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		ErrorLog: log.New(slogWriter{logger: logger}, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.InfoContext(ctx, "HTTP 服务开始监听", "addr", listener.Addr().String())
	// 后台额度刷新与 HTTP 服务并行运行：退出信号会取消它，serve 返回时也通过 stopRefresh 取消（异常退出时 ctx 未取消）。
	// 它不参与 HTTP 关闭等待，也与会话亲和的保存互不影响。
	refreshCtx, stopRefresh := context.WithCancel(ctx)
	defer stopRefresh()
	go pool.RunQuotaRefresh(refreshCtx)
	// 无论因退出信号还是异常退出，都要等 HTTP 服务关闭（无在途请求）之后再保存会话亲和。
	defer saveSessionAffinity(pool, config.SessionAffinity, logger)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		if err != nil {
			logger.ErrorContext(ctx, "HTTP 服务异常退出", "error", err)
		}
		// Serve 已异常返回，但已建立的连接可能仍有在途请求，关闭服务并等待其结束。
		_ = shutdownServer(server)
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，正在关闭 HTTP 服务")
		err := shutdownServer(server)
		<-done
		logger.Info("HTTP 服务已关闭")
		return err
	}
}

// shutdownServer 优雅关闭 HTTP 服务并等待在途请求结束，最多等待 10 秒；超时后强制关闭。
func shutdownServer(server *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := server.Shutdown(shutdownCtx)
	if err != nil {
		_ = server.Close()
	}
	return err
}

// saveSessionAffinity 在服务关闭后保存会话亲和绑定；未开启会话亲和时不做任何事。
// 成功输出 Info，失败只输出 Warn，不改变 serve 的返回值。
func saveSessionAffinity(pool *copilot.Pool, enabled bool, logger *slog.Logger) {
	if !enabled {
		return
	}
	if err := pool.SaveSessionAffinity(); err != nil {
		logger.Warn("会话亲和：保存失败", "error", err)
		return
	}
	logger.Info("会话亲和：已保存")
}

type slogWriter struct {
	logger *slog.Logger
}

func (w slogWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		w.logger.Error("HTTP 服务内部错误", "message", message)
	}
	return len(p), nil
}
