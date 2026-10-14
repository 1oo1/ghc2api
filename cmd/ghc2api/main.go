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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	configPath, err := configPath(args)
	if err != nil {
		return err
	}
	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	logger, err := newLogger(config.LogLevel, stderr)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "配置加载完成", "config_file", configPath, "log_level", config.LogLevel)
	if config.insecurePermissions {
		logger.WarnContext(ctx, "配置文件权限过宽，建议执行 chmod 600", "config_file", configPath,
			"mode", fmt.Sprintf("%04o", config.configMode))
	}
	if config.insecureCredential {
		logger.WarnContext(ctx, "凭证文件权限过宽，建议执行 chmod 600", "credential_file", config.credentialFile,
			"mode", fmt.Sprintf("%04o", config.credentialMode))
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
	service, err := copilot.New(copilot.Config{
		Token: config.Token, Host: config.Host, APIKey: config.APIKey,
		ModelsCacheTTL: config.ModelsCacheTTL, Logger: logger,
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("无法监听 %s: %w", config.Listen, err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		ErrorLog: log.New(slogWriter{logger: logger}, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.InfoContext(ctx, "HTTP 服务开始监听", "addr", listener.Addr().String())
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		if err != nil {
			logger.ErrorContext(ctx, "HTTP 服务异常退出", "error", err)
		}
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，正在关闭 HTTP 服务")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		<-done
		logger.Info("HTTP 服务已关闭")
		return err
	}
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
