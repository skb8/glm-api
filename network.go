package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func upstreamTransport(proxyAddress string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 20 * time.Second
	if proxyAddress != "" {
		proxy, err := url.Parse(proxyAddress)
		if err != nil || proxy.Hostname() == "" || (proxy.Path != "" && proxy.Path != "/") ||
			proxy.RawQuery != "" || proxy.Fragment != "" {
			return nil, errors.New("укажите адрес прокси в формате http://host:port или socks5://host:port")
		}
		switch proxy.Scheme {
		case "http", "https", "socks5", "socks5h":
			transport.Proxy = http.ProxyURL(proxy)
		default:
			return nil, errors.New("поддерживаемые схемы прокси: http, https, socks5 и socks5h")
		}
	}
	transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
		if response.StatusCode != http.StatusOK {
			return proxyConnectError(response.StatusCode)
		}
		return nil
	}
	return transport, nil
}

type proxyConnectError int

func (e proxyConnectError) Error() string {
	if e == http.StatusProxyAuthRequired {
		return "прокси требует аутентификацию (HTTP 407)"
	}
	return "прокси отклонил CONNECT (HTTP " + strconv.Itoa(int(e)) + ")"
}

type networkError struct {
	host      string
	detail    string
	temporary bool
}

func (e *networkError) Error() string {
	return fmt.Sprintf("соединение с %s: %s", e.host, e.detail)
}

func upstreamNetworkError(host string, err error) error {
	var (
		proxy     proxyConnectError
		dns       *net.DNSError
		netErr    net.Error
		op        *net.OpError
		tlsCert   *tls.CertificateVerificationError
		tlsRecord tls.RecordHeaderError
		x509CA    x509.UnknownAuthorityError
		x509CN    x509.HostnameError
		x509Bad   x509.CertificateInvalidError
	)
	detail := "сетевая ошибка"
	temporary := false
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.As(err, &proxy):
		detail = proxy.Error()
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		detail, temporary = "истекло время ожидания", true
	case errors.As(err, &dns):
		detail, temporary = "ошибка DNS", !dns.IsNotFound
	case errors.As(err, &tlsCert), errors.As(err, &x509CA), errors.As(err, &x509CN), errors.As(err, &x509Bad):
		detail = "проверка TLS-сертификата не прошла"
	case errors.As(err, &tlsRecord), errors.Is(err, http.ErrSchemeMismatch):
		detail = "не удалось установить TLS-соединение"
	case errors.Is(err, syscall.ECONNREFUSED):
		detail, temporary = "сервер отклонил соединение", true
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		detail, temporary = "сеть недоступна", true
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		detail, temporary = "соединение было прервано", true
	case errors.As(err, &op) && strings.HasPrefix(op.Op, "proxyconnect"):
		detail = "не удалось подключиться через сетевой прокси"
	}
	return &networkError{host: host, detail: detail, temporary: temporary}
}
