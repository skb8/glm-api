package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8787", "HTTP listen address")
	envPath := flag.String("env", ".env", "path to the credentials file")
	login := flag.Bool("login", false, "sign in to Z.AI again")
	noBrowser := flag.Bool("no-browser", false, "print the sign-in URL without opening a browser")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	env, err := readEnv(*envPath)
	if err != nil {
		return err
	}
	if env.get("GLM_API_KEY") == "" {
		key, err := randomHex()
		if err != nil {
			return err
		}
		env.set("GLM_API_KEY", "sk-glm-"+key)
	}
	if err := env.save(); err != nil {
		return err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	z := &zcode{http: client, oauthBase: zcodeBase, apiBase: zaiBase}
	freshLogin := *login || env.get("ZAI_ACCESS_TOKEN") == ""
	for {
		if freshLogin {
			tokens, err := z.login(ctx, func(url string) {
				fmt.Println("Войдите в Z.AI:", url)
				if !*noBrowser {
					openBrowser(url)
				}
			})
			if err != nil {
				return err
			}
			env.set("ZAI_ACCESS_TOKEN", tokens.Access)
			env.set("ZAI_REFRESH_TOKEN", tokens.Refresh)
			env.set("ZCODE_SESSION_TOKEN", tokens.Session)
			if err := env.save(); err != nil {
				return err
			}
		}
		z.accountToken = env.get("ZAI_ACCESS_TOKEN")
		if _, err := z.projectToken(ctx); err != nil {
			if errors.Is(err, errLoginRequired) && !freshLogin {
				fmt.Println("Сессия Z.AI истекла. Войдите снова.")
				freshLogin = true
				continue
			}
			return err
		}
		break
	}

	api := &server{z: z, key: env.get("GLM_API_KEY"), models: catalog}
	srv := &http.Server{
		Handler:           api.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	fmt.Printf("Сервер: http://%s\n", listener.Addr())
	fmt.Println("API-ключ:", api.key)
	fmt.Println("Модели ZCode Coding Plan:", strings.Join(api.models, ", "))
	if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func randomHex() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func openBrowser(url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if command.Start() == nil {
		go func() { _ = command.Wait() }()
	}
}
