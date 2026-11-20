package openaitransport

import (
	utls "github.com/refraction-networking/utls"
)

// rustlsCipherSuites 复刻 rustls(ring) 默认套件顺序：TLS1.3 三条在前，
// 其后是 ECDHE-ECDSA / ECDHE-RSA 的 AES-GCM 与 ChaCha20。
var rustlsCipherSuites = []uint16{
	utls.TLS_AES_256_GCM_SHA384,
	utls.TLS_AES_128_GCM_SHA256,
	utls.TLS_CHACHA20_POLY1305_SHA256,
	utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
}

// rustlsSignatureAlgorithms 是 rustls 默认的签名算法列表顺序。
var rustlsSignatureAlgorithms = []utls.SignatureScheme{
	utls.ECDSAWithP256AndSHA256,
	utls.Ed25519,
	utls.PSSWithSHA256,
	utls.PSSWithSHA384,
	utls.PSSWithSHA512,
	utls.PKCS1WithSHA256,
	utls.PKCS1WithSHA384,
	utls.PKCS1WithSHA512,
}

// rustlsCurves 是 rustls 默认的命名组顺序。
var rustlsCurves = []utls.CurveID{
	utls.X25519,
	utls.CurveP256,
	utls.CurveP384,
}

// rustlsClientHelloSpec 构造一个不含 GREASE、扩展顺序贴近 rustls 的 ClientHello。
//
// 说明：utls 会按 TLS 版本要求补齐/调整部分扩展（如 generating key_share 的
// 公钥、supported_versions 的编码），这里声明的是「顺序 + 内容」的意图；最终线序
// 必须以抓包断言为准（见 README 的验收脚本），不要以本文件为唯一真相。
func rustlsClientHelloSpec(alpns []string) *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
		CipherSuites:       rustlsCipherSuites,
		CompressionMethods: []uint8{0x00},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: rustlsCurves},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0x00}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: rustlsSignatureAlgorithms},
			&utls.ALPNExtension{AlpnProtocols: alpns},
			&utls.StatusRequestExtension{},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// alpnFor 返回与协议栈选择匹配的 ALPN 列表。
func alpnFor(http2 bool) []string {
	if http2 {
		return []string{"h2", "http/1.1"}
	}
	return []string{"http/1.1"}
}
