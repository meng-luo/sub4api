package service

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImagesJSONComplexityForwarding(t *testing.T) {
	for _, complexity := range []string{"low", "medium", "high"} {
		for _, endpoint := range []string{openAIImagesGenerationsEndpoint, openAIImagesEditsEndpoint} {
			t.Run(complexity+endpoint, func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-image-2.6-flare","prompt":"draw","complexity":%q,"images":[{"image_url":"https://example.com/input.png"}]}`, complexity))
				c, _ := newOpenAIImagesTestContext(t, body)
				c.Request.URL.Path = endpoint
				parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				require.True(t, parsed.HasNativeOptions)
				upstream, target, err := buildOpenAIImagesOAuthPayload(parsed, parsed.Model)
				require.NoError(t, err)
				require.Equal(t, "https://chatgpt.com/backend-api/codex"+endpoint[3:], target)
				require.Equal(t, complexity, gjson.GetBytes(upstream, "complexity").String())
				bridge, err := buildOpenAIImagesResponsesRequest(parsed, parsed.Model)
				require.NoError(t, err)
				require.Equal(t, complexity, gjson.GetBytes(bridge, "tools.0.complexity").String())
			})
		}
	}
}

func TestOpenAIImagesJSONRejectsInvalidComplexity(t *testing.T) {
	for _, value := range []string{`""`, `"auto"`, `"xhigh"`, `123`, `true`, `null`, `{}`, `[]`} {
		t.Run(value, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-image-2.6-flare","prompt":"draw","complexity":%s}`, value))
			c, _ := newOpenAIImagesTestContext(t, body)
			_, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
			require.ErrorContains(t, err, "complexity")
		})
	}
}

func TestOpenAIImagesMultipartComplexity(t *testing.T) {
	for _, complexity := range []string{"low", "medium", "high", "", "auto"} {
		t.Run(complexity, func(t *testing.T) {
			var body bytes.Buffer
			w := multipart.NewWriter(&body)
			require.NoError(t, w.WriteField("model", "gpt-image-2.6-sunburst"))
			require.NoError(t, w.WriteField("prompt", "edit"))
			require.NoError(t, w.WriteField("complexity", complexity))
			image, err := w.CreateFormFile("image", "image.png")
			require.NoError(t, err)
			_, err = image.Write([]byte("png-content"))
			require.NoError(t, err)
			require.NoError(t, w.Close())
			c, _ := newOpenAIImagesTestContext(t, body.Bytes())
			c.Request.URL.Path = openAIImagesEditsEndpoint
			c.Request.Header.Set("Content-Type", w.FormDataContentType())
			parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body.Bytes())
			if complexity == "" || complexity == "auto" {
				require.ErrorContains(t, err, "complexity")
				return
			}
			require.NoError(t, err)
			require.True(t, parsed.HasNativeOptions)
			upstream, target, err := buildOpenAIImagesOAuthPayload(parsed, parsed.Model)
			require.NoError(t, err)
			require.Equal(t, "https://chatgpt.com/backend-api/codex/images/edits", target)
			require.Equal(t, complexity, gjson.GetBytes(upstream, "complexity").String())
			require.Len(t, gjson.GetBytes(upstream, "images").Array(), 1)
			bridge, err := buildOpenAIImagesResponsesRequest(parsed, parsed.Model)
			require.NoError(t, err)
			require.Equal(t, complexity, gjson.GetBytes(bridge, "tools.0.complexity").String())
		})
	}
}

func TestOpenAIImagesComplexityRequiresNativeCapability(t *testing.T) {
	body := []byte(`{"prompt":"draw","complexity":"low"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", parsed.Model)
	require.Equal(t, OpenAIImagesCapabilityNative, parsed.RequiredCapability)
}

func TestOpenAIImageOnlyTransformPreservesComplexity(t *testing.T) {
	for _, existing := range []string{"", "high"} {
		t.Run(existing, func(t *testing.T) {
			req := map[string]any{
				"model":      "gpt-image-2.6-sunburst",
				"input":      "draw",
				"complexity": "low",
			}
			want := "low"
			if existing != "" {
				want = existing
				req["tools"] = []any{map[string]any{"type": "image_generation", "complexity": existing}}
			}
			require.True(t, normalizeOpenAIResponsesImageOnlyModel(req))
			require.NotContains(t, req, "complexity")
			tools, ok := req["tools"].([]any)
			require.True(t, ok)
			require.Len(t, tools, 1)
			tool, ok := tools[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "gpt-image-2.6-sunburst", tool["model"])
			require.Equal(t, want, tool["complexity"])
			require.False(t, normalizeOpenAIResponsesImageOnlyModel(req))
		})
	}
}
