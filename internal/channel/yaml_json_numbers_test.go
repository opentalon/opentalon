package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

const bigID = "1234567890123456789"

func TestDecodeJSONObject(t *testing.T) {
	obj, err := decodeJSONObject([]byte(`{"id":` + bigID + `,"n":{"x":9007199254740993}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if obj["id"] != json.Number(bigID) {
		t.Errorf("id = %#v", obj["id"])
	}

	obj, err = decodeJSONObject([]byte(`null`))
	if err != nil || obj != nil {
		t.Errorf("null: obj=%v err=%v", obj, err)
	}

	for _, bad := range []string{``, `   `, `[1,2]`, `"s"`, `{"a":1}]`, `{"a":1}}`, `{"a":1} x`, `{"a":1}{"b":2}`, `{"a":`} {
		if _, err := decodeJSONObject([]byte(bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestJSONNumbersRenderAsSent(t *testing.T) {
	obj, err := decodeJSONObject([]byte(`{"big":` + bigID + `,"int":42,"float":1.5,"one":1.0,"exp":1e3,"neg":-7}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"big": bigID, "int": "42", "float": "1.5", "one": "1.0", "exp": "1e3", "neg": "-7"}
	flat := flattenToStringMap(obj)
	for k, v := range want {
		if flat[k] != v {
			t.Errorf("flattenToStringMap[%s] = %q, want %q", k, flat[k], v)
		}
		if got := getStringField(obj, k); got != v {
			t.Errorf("getStringField(%s) = %q, want %q", k, got, v)
		}
	}
}

func dispatchRecorder(t *testing.T, ch *YAMLChannel) <-chan string {
	t.Helper()
	calls := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- string(body)
	}))
	t.Cleanup(srv.Close)
	ch.spec.Inbound.Dispatch = &DispatchSpec{
		When: []ProcessRule{{Field: "id", Equals: bigID}},
		Call: HTTPCallSpec{Method: "POST", URL: srv.URL, Body: `{"id":"{{event.id}}","mr":"{{event.merge_request.iid}}"}`},
	}
	return calls
}

func expectDispatch(t *testing.T, calls <-chan string) {
	t.Helper()
	want := `{"id":"` + bigID + `","mr":"9007199254740993"}`
	select {
	case got := <-calls:
		if got != want {
			t.Errorf("dispatch body = %q, want %q", got, want)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dispatch call was not made")
	}
}

const bigEvent = `{"object_kind":"note","id":` + bigID + `,"merge_request":{"iid":9007199254740993}}`

func TestPollPreservesLargeIntegerIDs(t *testing.T) {
	ch := newTestChannel([]string{"note"}, make(chan pkg.InboundMessage, 1))
	calls := dispatchRecorder(t, ch)
	poll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":9007199254740993,"event":`+bigEvent+`}]}`)
	}))
	defer poll.Close()
	ch.spec.Inbound.Polling = &PollingInboundSpec{URL: poll.URL, ResultPath: "result", CursorField: "update_id"}
	ch.spec.Inbound.EventPath = "event"

	ch.doPollOnce(http.MethodGet, poll.Client())
	expectDispatch(t, calls)
	if got := ch.selfVars["poll_offset"]; got != "9007199254740994" {
		t.Errorf("poll_offset = %q, want 9007199254740994", got)
	}
}

func TestHandleFramePreservesLargeIntegerIDs(t *testing.T) {
	ch := newTestChannel([]string{"note"}, make(chan pkg.InboundMessage, 1))
	calls := dispatchRecorder(t, ch)
	ch.handleFrame(nil, []byte(bigEvent))
	expectDispatch(t, calls)
}

func TestRunInitStoresLargeIntegerIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user":{"id":`+bigID+`}}`)
	}))
	defer srv.Close()
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	if err := ch.runInit([]InitStep{{Name: "me", Method: "GET", URL: srv.URL, Store: map[string]string{"bot_id": "user.id"}}}); err != nil {
		t.Fatal(err)
	}
	if got := ch.selfVars["bot_id"]; got != bigID {
		t.Errorf("self.bot_id = %q, want %s", got, bigID)
	}
}

func TestCaptureFieldAndEnrichKeepLargeIntegerIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"note":{"id":`+bigID+`}}`)
	}))
	defer srv.Close()
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	ctx := context.Background()

	got, err := ch.doHTTPCallCaptureField(ctx, HTTPCallSpec{Method: "POST", URL: srv.URL}, map[string]map[string]string{}, "note.id")
	if err != nil || got != bigID {
		t.Errorf("captured field = %q, %v", got, err)
	}

	fields, err := ch.callEnrich(ctx, EnrichSpec{Method: "GET", URL: srv.URL, Extract: map[string]string{"id": "note.id"}}, map[string]map[string]string{})
	if err != nil || fields["id"] != bigID {
		t.Errorf("enrich extract = %v, %v", fields, err)
	}
}

func TestWebhookSkipsInvalidTrailingJSON(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel(nil, inbox)
	ch.processInboundData([]byte(`{"text":"hi","conversation":{"id":"c"},"from":{"id":"u"}}]`))
	select {
	case m := <-inbox:
		t.Fatalf("a body with trailing data must be dropped, got %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}
