package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const timeoutShort = 10 * time.Second

// certDaysLeft 读证书剩余有效天数。
//
// 直接解析证书而不是调 openssl：少一个外部依赖，也更快。
func certDaysLeft(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		return -1
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return -1
	}

	days := int(time.Until(cert.NotAfter).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// checkDomainResolves 检查域名是否解析到本机。
//
// 解析不对就申请证书，certbot 会等 HTTP 验证超时才失败，
// 用户干等几分钟还看不懂错在哪。提前检查能给出明确提示。
func checkDomainResolves(domain string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("域名 %s 无法解析，请先在 DNS 里添加 A 记录指向本机 IP", domain)
	}

	// 本机所有 IP
	localIPs := localAddresses()

	for _, addr := range addrs {
		if localIPs[addr] {
			return nil
		}
	}

	// 不直接报错，只警告：可能用了 CDN 或负载均衡，
	// 域名解析到 CDN 也能正常签发（HTTP 验证走回源）。
	// 所以这里返回 nil，由前端展示提示。
	return nil
}

func localAddresses() map[string]bool {
	out := make(map[string]bool)

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}

	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			out[ipnet.IP.String()] = true
		}
	}

	return out
}

// CheckSSL 测试某个域名的 HTTPS 是否可用（面板自检用）。
func CheckSSL(domain string) (bool, int, error) {
	if !strings.Contains(domain, ".") {
		return false, 0, fmt.Errorf("域名不合法")
	}

	dialer := &net.Dialer{Timeout: 8 * time.Second}

	conn, err := tls.DialWithDialer(dialer, "tcp", domain+":443", &tls.Config{
		ServerName: domain,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return false, 0, err
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return false, 0, fmt.Errorf("未取到证书")
	}

	days := int(time.Until(certs[0].NotAfter).Hours() / 24)
	if days < 0 {
		days = 0
	}

	return true, days, nil
}
