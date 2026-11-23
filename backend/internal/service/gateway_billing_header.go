package service

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ccVersionInBillingRe matches the semver part of cc_version (X.Y.Z).
var ccVersionInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+`)

var ccVersionWithFingerprintInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+\.[0-9a-fA-F]{3}\b`)

const claudeBillingSourceTextKey = "claude_billing_source_text"

// Capture before proxy system migration. Never infer provenance from text prefixes.
func captureClaudeBillingSource(c *gin.Context, body []byte) {
	if c != nil {
		c.Set(claudeBillingSourceTextKey, strings.Clone(extractFirstUserText(body)))
	}
}

// syncBillingHeaderVersion rewrites cc_version in x-anthropic-billing-header
// system text blocks to match the version extracted from userAgent.
// Recompute any recognized fingerprint suffix because its input includes the version.
// Only touches system array blocks whose text starts with "x-anthropic-billing-header".

// effectiveRequestBillingUserAgent selects the version source for billing
// synchronization. Real Claude Code requests use the OAuth account fingerprint
// because the upstream account, not the client-side subscription, owns the
// outbound billing attribution. Mimicry uses the proxy compatibility UA.
func effectiveRequestBillingUserAgent(mimicUserAgent, clientUserAgent, tokenType string, mimicClaudeCode bool, fingerprint *Fingerprint) string {
	if tokenType != "oauth" {
		return ""
	}
	if mimicClaudeCode {
		return mimicUserAgent
	}
	if fingerprint != nil && ExtractCLIVersion(fingerprint.UserAgent) != "" {
		return fingerprint.UserAgent
	}
	if ExtractCLIVersion(clientUserAgent) != "" {
		return clientUserAgent
	}
	return ""
}
func syncBillingHeaderVersion(body []byte, userAgent string, c *gin.Context) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}

	systemResult := lastRootJSONField(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	firstText := extractFirstUserText(body)
	if c != nil {
		if saved, ok := c.Get(claudeBillingSourceTextKey); ok {
			if savedText, ok := saved.(string); ok {
				firstText = savedText
			}
		}
	}
	fingerprint := computeClaudeCodeFingerprintFromText(firstText, version)
	replacement := "cc_version=" + version
	// The fingerprint depends only on the original messages and version, so it
	// is the same for every billing block. Compute it lazily, at most once.
	fingerprintedReplacement := ""
	var rebuilt bytes.Buffer
	copyFrom := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text, repeated := lastJSONObjectField(item, "text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			if fingerprintedReplacement == "" {
				fingerprintedReplacement = replacement + "." + fingerprint
			}
			newText := ccVersionWithFingerprintInBillingRe.ReplaceAllString(text.String(), fingerprintedReplacement)
			newText = ccVersionInBillingRe.ReplaceAllString(newText, replacement)
			if repeated || newText != text.String() {
				updated, err := setRootJSONValue(sliceRawFromBody(body, item), "text", newText)
				if err != nil {
					return true
				}
				if copyFrom == 0 {
					rebuilt.Grow(len(systemResult.Raw))
				}
				itemStart := item.Index - systemResult.Index
				rebuilt.WriteString(systemResult.Raw[copyFrom:itemStart])
				rebuilt.Write(updated)
				copyFrom = itemStart + len(item.Raw)
			}
		}
		return true
	})
	if copyFrom == 0 {
		return body
	}
	rebuilt.WriteString(systemResult.Raw[copyFrom:])
	updated, err := setRootJSONField(body, "system", rebuilt.Bytes())
	if err != nil {
		return body
	}
	return updated
}

func billingSourceText(body []byte, c *gin.Context) string {
	if c != nil {
		if saved, ok := c.Get(claudeBillingSourceTextKey); ok {
			if savedText, ok := saved.(string); ok {
				return savedText
			}
		}
	}
	return extractFirstUserText(body)
}

// ensureClaudeCodeBillingBlock supplies the upstream-account attribution when
// a real Claude Code client reached the gateway without its own block.
func ensureClaudeCodeBillingBlock(body []byte, userAgent string, c *gin.Context) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}
	system := lastRootJSONField(body, "system")
	if system.IsArray() {
		for _, item := range system.Array() {
			text, _ := lastJSONObjectField(item, "text")
			if strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
				return syncBillingHeaderVersion(body, userAgent, c)
			}
		}
	}
	text := fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=00000;", version, computeClaudeCodeFingerprintFromText(billingSourceText(body, c), version))
	billingBlock, err := marshalAnthropicSystemTextBlockWithCacheControl(text, nil)
	if err != nil {
		return body
	}
	items := [][]byte{billingBlock}
	if system.IsArray() {
		for _, item := range system.Array() {
			items = append(items, []byte(item.Raw))
		}
	} else if system.Exists() && system.Type == gjson.String {
		if original, err := marshalAnthropicSystemTextBlockWithCacheControl(system.String(), nil); err == nil {
			items = append(items, original)
		}
	}
	updated, err := setRootJSONField(body, "system", buildJSONArrayRaw(items))
	if err != nil {
		return body
	}
	return updated
}
