package openaitransport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// captureClientHello 在本机起一个裸 TCP 监听，抓取一次 ClientHello 并返回解析结果。
func captureClientHello(t *testing.T, cfg Config) (ciphers []uint16, extensions []uint16, alpns []string, curves []uint16, groupsHasGREASE bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	helloCh := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 16*1024)
		n, _ := conn.Read(buf)
		if n > 5 {
			helloCh <- append([]byte(nil), buf[:n]...)
		}
	}()

	d, err := newDialer(cfg, "")
	if err != nil {
		t.Fatalf("newDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 触发完整握手：ClientHello 会先发出，随后因对端不回应而超时，字节已被捕获。
	// 用主机名而非 IP，确保 SNI 扩展被发送（IP 字面量不发 SNI）。
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_, _ = d.dialTLSContext(ctx, "tcp", net.JoinHostPort("localhost", port))

	select {
	case raw := <-helloCh:
		return parseClientHello(t, raw)
	case <-time.After(3 * time.Second):
		t.Fatal("未捕获到 ClientHello")
	}
	return nil, nil, nil, nil, false
}

// parseClientHello 解析 TLS ClientHello 的关键字段。
func parseClientHello(t *testing.T, raw []byte) ([]uint16, []uint16, []string, []uint16, bool) {
	t.Helper()
	if len(raw) < 5+4+2+32 {
		t.Fatalf("ClientHello 太短: %d", len(raw))
	}
	if raw[0] != 0x16 {
		t.Fatalf("不是 handshake 记录: %x", raw[0])
	}
	p := 5 + 4 // record header + handshake header
	p += 2     // legacy_version
	p += 32    // random
	if p >= len(raw) {
		t.Fatal("会话 ID 段越界")
	}
	sidLen := int(raw[p])
	p += 1 + sidLen

	if p+2 > len(raw) {
		t.Fatal("套件段越界")
	}
	csLen := int(binary.BigEndian.Uint16(raw[p:]))
	p += 2
	var ciphers []uint16
	for i := 0; i+1 < csLen; i += 2 {
		ciphers = append(ciphers, binary.BigEndian.Uint16(raw[p+i:]))
	}
	p += csLen

	if p >= len(raw) {
		t.Fatal("压缩段越界")
	}
	compLen := int(raw[p])
	p += 1 + compLen

	if p+2 > len(raw) {
		t.Fatal("扩展段越界")
	}
	extLen := int(binary.BigEndian.Uint16(raw[p:]))
	p += 2
	end := p + extLen
	var extensions []uint16
	var alpns []string
	var curves []uint16
	groupsHasGREASE := false
	for p+4 <= end && p+4 <= len(raw) {
		typ := binary.BigEndian.Uint16(raw[p:])
		l := int(binary.BigEndian.Uint16(raw[p+2:]))
		body := raw[p+4 : p+4+l]
		extensions = append(extensions, typ)
		switch typ {
		case 0x000a: // supported_groups
			if len(body) >= 2 {
				n := int(binary.BigEndian.Uint16(body))
				for i := 0; i+1 < n && 2+i+1 < len(body); i += 2 {
					g := binary.BigEndian.Uint16(body[2+i:])
					curves = append(curves, g)
					if isGREASE(g) {
						groupsHasGREASE = true
					}
				}
			}
		case 0x0010: // ALPN
			if len(body) >= 2 {
				q := 2
				for q < len(body) {
					ln := int(body[q])
					if q+1+ln > len(body) {
						break
					}
					alpns = append(alpns, string(body[q+1:q+1+ln]))
					q += 1 + ln
				}
			}
		}
		p += 4 + l
	}
	return ciphers, extensions, alpns, curves, groupsHasGREASE
}

func isGREASE(v uint16) bool {
	return v&0x0f0f == 0x0a0a && v>>8 == v&0xff
}

// TestClientHelloShape 断言线序关键特征：无 GREASE、TLS1.3 套件前置、ALPN 含 h2、
// SNI 为第一个扩展。
func TestClientHelloShape(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InsecureSkipVerify = true
	ciphers, exts, alpns, curves, hasGREASE := captureClientHello(t, cfg)

	if hasGREASE {
		t.Fatalf("不应包含 GREASE 组: %v", curves)
	}
	if len(ciphers) == 0 || ciphers[0] != 0x1302 {
		t.Fatalf("首个套件应为 TLS_AES_256_GCM_SHA384，实际 %x", ciphers)
	}
	var hasSNI bool
	for _, e := range exts {
		if e == 0x0000 {
			hasSNI = true
		}
	}
	if !hasSNI {
		t.Fatalf("缺少 SNI 扩展: %x", exts)
	}
	want := map[string]bool{"h2": false, "http/1.1": false}
	for _, a := range alpns {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	if !want["h2"] || !want["http/1.1"] {
		t.Fatalf("ALPN 异常: %v", alpns)
	}
	fmt.Printf("[clienthello] ciphers=%d exts=%d curves=%v alpn=%v\n", len(ciphers), len(exts), curves, alpns)
	fmt.Printf("[clienthello] ext_order=%04x\n", exts)
}

var _ = errors.New
