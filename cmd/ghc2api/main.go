package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "help", "--help", "-h":
		_, err := fmt.Fprintln(stdout, "用法：ghc2api serve [选项] | ghc2api login [选项]\n使用子命令 --help 查看选项。")
		return err
	case "login":
		flags := flag.NewFlagSet("login", flag.ContinueOnError)
		flags.SetOutput(stderr)
		host := flags.String("host", os.Getenv("COPILOT_HOST"), "GitHub 或 <name>.ghe.com 登录域名")
		path := flags.String("credentials", "credentials.json", "保存登录凭证的文件")
		if err := flags.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("login 不接受额外的位置参数")
		}
		return copilot.Login(ctx, *host, *path, nil, stdout)
	case "serve":
		return serve(ctx, args[1:], stdout, stderr)
	default:
		return errors.New("未知子命令，请使用 serve、login 或 help")
	}
}

func loadConfig(credentialsPath, editorConfig, host, mode string) (copilot.Config, error) {
	c := copilot.Credential{Token: strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))}
	if c.Token == "" {
		var err error
		switch {
		case credentialsPath != "":
			c, err = copilot.LoadCredential(credentialsPath)
		case editorConfig != "":
			c, err = copilot.DiscoverEditor(editorConfig)
		default:
			return copilot.Config{}, errors.New("未配置 token，请先执行 login 或设置 GITHUB_TOKEN")
		}
		if err != nil {
			return copilot.Config{}, err
		}
	}
	if host != "" {
		c.Host = host
	}
	if mode != "" {
		c.Mode = mode
	}
	normalized, err := copilot.NormalizeHost(c.Host)
	if err != nil {
		return copilot.Config{}, err
	}
	config := copilot.Config{Token: c.Token, Host: normalized, TokenMode: c.Mode, APIKey: strings.TrimSpace(os.Getenv("GHC2API_API_KEY"))}
	if _, err := copilot.New(config); err != nil {
		return copilot.Config{}, err
	}
	return config, nil
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("listen", "127.0.0.1:8080", "HTTP 监听地址，非本机地址必须配置 GHC2API_API_KEY")
	path := flags.String("credentials", "", "凭证文件，未指定时尝试 ./credentials.json")
	editor := flags.String("editor-config", "", "包含 github-copilot 目录的编辑器配置目录（只读）")
	host := flags.String("host", os.Getenv("COPILOT_HOST"), "覆盖凭证中的 GitHub 登录域名")
	mode := flags.String("token-mode", os.Getenv("COPILOT_TOKEN_MODE"), "token 模式：oauth 或 cli")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve 不接受额外的位置参数")
	}
	if *path == "" {
		if _, err := os.Stat("credentials.json"); err == nil {
			*path = "credentials.json"
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("无法读取默认凭证文件: %w", err)
		}
	}
	config, err := loadConfig(*path, *editor, *host, *mode)
	if err != nil {
		return err
	}
	listenHost, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(listenHost)
	local := (ip != nil && ip.IsLoopback()) || listenHost == "localhost"
	if !local && config.APIKey == "" {
		return errors.New("非本机监听地址必须配置 GHC2API_API_KEY 服务访问密钥")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	service, err := copilot.New(config)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	if tcp, ok := listener.Addr().(*net.TCPAddr); config.APIKey == "" && (!ok || !tcp.IP.IsLoopback()) {
		return errors.New("实际监听地址不是本机地址，必须配置 GHC2API_API_KEY")
	}
	server := &http.Server{
		Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute,
		ErrorLog: log.New(stderr, "HTTP 服务: ", log.LstdFlags),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	_, _ = fmt.Fprintf(stdout, "Copilot HTTP 服务监听 http://%s\n", listener.Addr())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		<-done
		return err
	}
}
