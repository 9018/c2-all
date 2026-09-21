package transport

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fetchECHViaUDP 普通 UDP DNS 查 HTTPS RR 取 ECH config（测试用）
func fetchECHViaUDP(t *testing.T, host string) []byte {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeHTTPS)
	c := new(dns.Client)
	c.Net = "udp"
	resp, _, err := c.Exchange(m, "1.1.1.1:53")
	if err != nil {
		t.Fatalf("dns: %v", err)
	}
	for _, rr := range resp.Answer {
		if h, ok := rr.(*dns.HTTPS); ok {
			for _, kv := range h.Value {
				if e, ok := kv.(*dns.SVCBECHConfig); ok && len(e.ECH) > 0 {
					return e.ECH
				}
			}
		}
	}
	return nil
}

func TestECHProbeLive(t *testing.T) {
	for _, host := range []string{"relay.zhpmt8ymbc.kdns.fr", "relay.j6k5as4z6k.kdns.fr"} {
		ech := fetchECHViaUDP(t, host)
		if ech == nil {
			t.Fatalf("[%s] no ech config in DNS", host)
		}
		pub, err := echPublicNameOf(ech)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ttl := time.Hour
		echStore(host, ech, pub, ttl)
		t.Logf("[%s] config injected: public_name=%s len=%d", host, pub, len(ech))

		dial := func() (net.Conn, error) {
			d := net.Dialer{Timeout: 8 * time.Second}
			return d.Dial("tcp", host+":443")
		}
		t0 := time.Now()
		conn, err := tlsConnectOnce(dial, host+":443", false)
		if err != nil {
			t.Logf("[%s] ECH FAILED after %v: %v", host, time.Since(t0).Round(time.Millisecond), err)
			continue
		}
		// 确认 TLS 握手完成且状态
		st := ECHStatus()
		conn.Close()
		t.Logf("[%s] ECH handshake OK in %v (status=%s)", host, time.Since(t0).Round(time.Millisecond), st)
	}
	fmt.Println("done")
}
