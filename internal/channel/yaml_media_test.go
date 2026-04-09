package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// --- getStringField array indexing ---

func TestGetStringField_ArrayFirstElement(t *testing.T) {
	m := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"id": "first"},
			map[string]interface{}{"id": "second"},
		},
	}
	if got := getStringField(m, "items.0.id"); got != "first" {
		t.Errorf("got %q, want %q", got, "first")
	}
}

func TestGetStringField_ArrayLastElement(t *testing.T) {
	m := map[string]interface{}{
		"photo": []interface{}{
			map[string]interface{}{"file_id": "small", "width": float64(90)},
			map[string]interface{}{"file_id": "medium", "width": float64(320)},
			map[string]interface{}{"file_id": "large", "width": float64(800)},
		},
	}
	if got := getStringField(m, "photo.-1.file_id"); got != "large" {
		t.Errorf("got %q, want %q", got, "large")
	}
}

func TestGetStringField_ArrayNegativeIndex(t *testing.T) {
	m := map[string]interface{}{
		"arr": []interface{}{
			map[string]interface{}{"v": "a"},
			map[string]interface{}{"v": "b"},
			map[string]interface{}{"v": "c"},
		},
	}
	if got := getStringField(m, "arr.-2.v"); got != "b" {
		t.Errorf("got %q, want %q", got, "b")
	}
}

func TestGetStringField_ArrayOutOfBounds(t *testing.T) {
	m := map[string]interface{}{
		"arr": []interface{}{"a", "b"},
	}
	if got := getStringField(m, "arr.5"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestGetStringField_ArrayScalarElement(t *testing.T) {
	m := map[string]interface{}{
		"tags": []interface{}{"go", "yaml", "test"},
	}
	if got := getStringField(m, "tags.1"); got != "yaml" {
		t.Errorf("got %q, want %q", got, "yaml")
	}
}

func TestGetStringField_NestedArrayPath(t *testing.T) {
	// Simulates Telegram photo message: message.photo.-1.file_id
	m := map[string]interface{}{
		"message": map[string]interface{}{
			"photo": []interface{}{
				map[string]interface{}{"file_id": "ABC123", "width": float64(90)},
				map[string]interface{}{"file_id": "DEF456", "width": float64(800)},
			},
		},
	}
	if got := getStringField(m, "message.photo.-1.file_id"); got != "DEF456" {
		t.Errorf("got %q, want %q", got, "DEF456")
	}
}

// --- navigateToValue ---

func TestNavigateToValue_TopLevelField(t *testing.T) {
	m := map[string]interface{}{"text": "hello"}
	if _, ok := navigateToValue(m, "text"); !ok {
		t.Error("expected text to exist")
	}
}

func TestNavigateToValue_MissingField(t *testing.T) {
	m := map[string]interface{}{"text": "hello"}
	if _, ok := navigateToValue(m, "photo"); ok {
		t.Error("expected photo to not exist")
	}
}

func TestNavigateToValue_Array(t *testing.T) {
	m := map[string]interface{}{
		"photo": []interface{}{
			map[string]interface{}{"file_id": "abc"},
		},
	}
	val, ok := navigateToValue(m, "photo")
	if !ok {
		t.Fatal("expected photo to exist")
	}
	if _, isArr := val.([]interface{}); !isArr {
		t.Error("expected photo to be an array")
	}
}

func TestNavigateToValue_NestedObject(t *testing.T) {
	m := map[string]interface{}{
		"sticker": map[string]interface{}{
			"emoji": "😂",
		},
	}
	if _, ok := navigateToValue(m, "sticker"); !ok {
		t.Error("expected sticker to exist")
	}
}

func TestNavigateToValue_NilValue(t *testing.T) {
	m := map[string]interface{}{"key": nil}
	if _, ok := navigateToValue(m, "key"); ok {
		t.Error("expected nil value to return false")
	}
}

// --- enrichEventCtx ---

func TestEnrichEventCtx_ResolvesNestedPaths(t *testing.T) {
	event := map[string]interface{}{
		"photo": []interface{}{
			map[string]interface{}{"file_id": "small123"},
			map[string]interface{}{"file_id": "large456"},
		},
		"voice": map[string]interface{}{
			"duration":  float64(5),
			"mime_type": "audio/ogg",
		},
	}
	eventCtx := flattenToStringMap(event)

	rule := MediaRule{
		Description: "[Photo: {{event.photo.-1.file_id}}, voice: {{event.voice.duration}}s]",
	}

	enrichEventCtx(event, eventCtx, rule)

	if got := eventCtx["photo.-1.file_id"]; got != "large456" {
		t.Errorf("photo.-1.file_id = %q, want %q", got, "large456")
	}
	if got := eventCtx["voice.duration"]; got != "5" {
		t.Errorf("voice.duration = %q, want %q", got, "5")
	}
}

func TestEnrichEventCtx_ResolvesFromResolveSteps(t *testing.T) {
	event := map[string]interface{}{
		"document": map[string]interface{}{
			"file_id":   "doc123",
			"file_name": "report.pdf",
			"mime_type": "application/pdf",
		},
	}
	eventCtx := flattenToStringMap(event)

	rule := MediaRule{
		Description: "[Document: {{event.document.file_name}}]",
		Resolve: &MediaResolveSpec{
			MimeType: "{{event.document.mime_type}}",
			Name:     "{{event.document.file_name}}",
			Steps: []MediaResolveStep{
				{
					URL: "https://example.com/getFile?file_id={{event.document.file_id}}",
				},
			},
		},
	}

	enrichEventCtx(event, eventCtx, rule)

	if got := eventCtx["document.file_id"]; got != "doc123" {
		t.Errorf("document.file_id = %q, want %q", got, "doc123")
	}
	if got := eventCtx["document.file_name"]; got != "report.pdf" {
		t.Errorf("document.file_name = %q, want %q", got, "report.pdf")
	}
	if got := eventCtx["document.mime_type"]; got != "application/pdf" {
		t.Errorf("document.mime_type = %q, want %q", got, "application/pdf")
	}
}

// --- resolveMedia ---

func TestResolveMedia_DescriptionOnlyRule(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Media: []MediaRule{
					{
						When:        "voice",
						Description: "[Voice message, {{event.voice.duration}}s. Type your message instead.]",
					},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	event := map[string]interface{}{
		"voice": map[string]interface{}{
			"duration":  float64(5),
			"mime_type": "audio/ogg",
		},
	}
	eventCtx := flattenToStringMap(event)
	msg := pkg.InboundMessage{Content: ""}

	ch.resolveMedia(event, eventCtx, &msg)

	if msg.Content != "[Voice message, 5s. Type your message instead.]" {
		t.Errorf("content = %q", msg.Content)
	}
	if len(msg.Files) != 0 {
		t.Errorf("expected no files, got %d", len(msg.Files))
	}
}

func TestResolveMedia_DoesNotOverrideExistingContent(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Media: []MediaRule{
					{
						When:        "photo",
						Description: "[Photo]",
					},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	event := map[string]interface{}{
		"photo": []interface{}{map[string]interface{}{"file_id": "abc"}},
	}
	eventCtx := flattenToStringMap(event)
	msg := pkg.InboundMessage{Content: "look at this!"}

	ch.resolveMedia(event, eventCtx, &msg)

	if msg.Content != "look at this!" {
		t.Errorf("content should not be overridden, got %q", msg.Content)
	}
}

func TestResolveMedia_NoMatchingRule(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Media: []MediaRule{
					{When: "photo", Description: "[Photo]"},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	event := map[string]interface{}{
		"text": "hello",
	}
	eventCtx := flattenToStringMap(event)
	msg := pkg.InboundMessage{Content: "hello"}

	ch.resolveMedia(event, eventCtx, &msg)

	if msg.Content != "hello" {
		t.Errorf("content should not change, got %q", msg.Content)
	}
}

func TestResolveMedia_FirstMatchWins(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Media: []MediaRule{
					{When: "sticker", Description: "[Sticker]"},
					{When: "photo", Description: "[Photo]"},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	// Event has both sticker and photo (unlikely but tests ordering)
	event := map[string]interface{}{
		"sticker": map[string]interface{}{"emoji": "😂"},
		"photo":   []interface{}{map[string]interface{}{"file_id": "abc"}},
	}
	eventCtx := flattenToStringMap(event)
	msg := pkg.InboundMessage{Content: ""}

	ch.resolveMedia(event, eventCtx, &msg)

	if msg.Content != "[Sticker]" {
		t.Errorf("first rule should win, got %q", msg.Content)
	}
}

func TestResolveMedia_WithResolveSteps(t *testing.T) {
	// Mock Telegram API: getFile returns file_path, then serve binary
	fileData := []byte{0x89, 0x50, 0x4E, 0x47} // fake PNG header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTOKEN/getFile":
			resp := map[string]interface{}{
				"ok": true,
				"result": map[string]interface{}{
					"file_path": "photos/test.jpg",
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/file/botTOKEN/photos/test.jpg":
			_, _ = w.Write(fileData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Media: []MediaRule{
					{
						When:        "photo",
						Description: "[Photo]",
						Resolve: &MediaResolveSpec{
							MimeType: "image/jpeg",
							Name:     "photo.jpg",
							Steps: []MediaResolveStep{
								{
									Method: "GET",
									URL:    server.URL + "/botTOKEN/getFile?file_id={{event.photo.-1.file_id}}",
									Store:  map[string]string{"file_path": "result.file_path"},
								},
								{
									Method: "GET",
									URL:    server.URL + "/file/botTOKEN/{{resolve.file_path}}",
								},
							},
						},
					},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
		client:   &http.Client{},
		ctx:      ctx,
	}

	event := map[string]interface{}{
		"photo": []interface{}{
			map[string]interface{}{"file_id": "small123", "width": float64(90)},
			map[string]interface{}{"file_id": "large456", "width": float64(800)},
		},
	}
	eventCtx := flattenToStringMap(event)
	msg := pkg.InboundMessage{Content: ""}

	ch.resolveMedia(event, eventCtx, &msg)

	if len(msg.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(msg.Files))
	}
	if msg.Files[0].MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", msg.Files[0].MimeType)
	}
	if msg.Files[0].Name != "photo.jpg" {
		t.Errorf("name = %q, want photo.jpg", msg.Files[0].Name)
	}
	if string(msg.Files[0].Data) != string(fileData) {
		t.Errorf("data mismatch")
	}
	// Content should be the description since original was empty
	if msg.Content != "[Photo]" {
		t.Errorf("content = %q, want [Photo]", msg.Content)
	}
}

func TestResolveMedia_ResolveFallsBackToDescription(t *testing.T) {
	// Server returns 500 for getFile — resolve should fail gracefully
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Media: []MediaRule{
					{
						When:        "photo",
						Description: "[Photo failed to load]",
						Resolve: &MediaResolveSpec{
							MimeType: "image/jpeg",
							Name:     "photo.jpg",
							Steps: []MediaResolveStep{
								{
									Method: "GET",
									URL:    server.URL + "/getFile",
									Store:  map[string]string{"file_path": "result.file_path"},
								},
							},
						},
					},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
		client:   &http.Client{},
		ctx:      ctx,
	}

	event := map[string]interface{}{
		"photo": []interface{}{map[string]interface{}{"file_id": "abc"}},
	}
	eventCtx := flattenToStringMap(event)
	msg := pkg.InboundMessage{Content: ""}

	ch.resolveMedia(event, eventCtx, &msg)

	if len(msg.Files) != 0 {
		t.Errorf("expected no files on resolve failure, got %d", len(msg.Files))
	}
	if msg.Content != "[Photo failed to load]" {
		t.Errorf("content = %q, want fallback description", msg.Content)
	}
}
