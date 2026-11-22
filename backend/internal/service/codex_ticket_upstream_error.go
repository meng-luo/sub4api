package service

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/MACOS-DO/sub4api/internal/util/logredact"
	"github.com/tidwall/gjson"
)

const codexTicketErrorBodyLimit = 16 << 10

var (
	codexTicketCredentialURL   = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	codexTicketBearer          = regexp.MustCompile(`(?i)\bBearer\s+[^\s"',;]+`)
	codexTicketSensitiveHeader = regexp.MustCompile(`(?im)\b((?:proxy-authorization|authorization|set-cookie|cookie|x-codex-turn-state)\s*[:=])\s*[^\r\n]+`)
)

// Only scalar fields from a bounded error response may enter the attempt history.
func readCodexTicketUpstreamError(resp *http.Response, token, state string) (code, message string, err error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, codexTicketErrorBodyLimit+1))
	if err != nil {
		return "", "Could not read the upstream error response.", err
	}
	if len(body) > codexTicketErrorBodyLimit {
		return "", "Upstream error response exceeded 16 KiB.", nil
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", "", nil
	}
	if !gjson.ValidBytes(body) {
		if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
			return "", "Upstream returned an HTML error page.", nil
		}
		return "", "Upstream returned a non-JSON error response.", nil
	}
	secrets := []string{token, state}
	for _, cookie := range resp.Cookies() {
		secrets = append(secrets, cookie.Value)
	}
	payload := gjson.ParseBytes(body)
	code = codexTicketErrorField(payload, "error.code", "detail.code", "code", "error.type", "detail.type")
	message = codexTicketErrorField(payload, "error.message", "detail.message", "detail.0.msg", "detail", "message", "error")
	return cleanCodexTicketErrorField(code, 128, secrets), cleanCodexTicketErrorField(message, 1024, secrets), nil
}

func codexTicketErrorField(payload gjson.Result, paths ...string) string {
	for _, path := range paths {
		field := payload.Get(path)
		if field.Type == gjson.String && strings.TrimSpace(field.Str) != "" {
			return field.Str
		}
	}
	return ""
}

func cleanCodexTicketErrorField(value string, maxBytes int, secrets []string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	// An upstream may echo credentials in otherwise ordinary error text.
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	value = codexTicketCredentialURL.ReplaceAllString(value, "***")
	value = codexTicketSensitiveHeader.ReplaceAllString(value, "${1} ***")
	value = codexTicketBearer.ReplaceAllString(value, "Bearer ***")
	value = redactContentModerationSecrets(value)
	value = logredact.RedactText(value, "api_key", "authorization", "cookie", "set-cookie", "x-codex-turn-state", "proxy-authorization", "token")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return truncateUTF8(strings.TrimSpace(value), maxBytes)
}
