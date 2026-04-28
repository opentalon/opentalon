package orchestrator

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// sttExecutor simulates an STT plugin: decodes file_data and returns a fixed transcript.
type sttExecutor struct {
	transcript string // returned on success
	err        string // if non-empty, returned as ToolResult.Error
}

func (e *sttExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	if e.err != "" {
		return ToolResult{CallID: call.ID, Error: e.err}
	}
	// Ensure file_data and file_mime args are present.
	if _, ok := call.Args["file_data"]; !ok {
		return ToolResult{CallID: call.ID, Error: "missing file_data arg"}
	}
	if _, ok := call.Args["file_mime"]; !ok {
		return ToolResult{CallID: call.ID, Error: "missing file_mime arg"}
	}
	return ToolResult{CallID: call.ID, Content: e.transcript}
}

func newSTTOrchestrator(sttExec PluginExecutor, sttPrep ContentPreparerEntry) *Orchestrator {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:    sttPrep.Plugin,
		Actions: []Action{{Name: sttPrep.Action, Description: "transcribe"}},
	}, sttExec)

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	return NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess,
		OrchestratorOpts{ContentPreparers: []ContentPreparerEntry{sttPrep}},
	)
}

func audioFile(mimeType string, data []byte) provider.MessageFile {
	return provider.MessageFile{MimeType: mimeType, Data: data}
}

// --- runSTTPreparers unit tests ---

func TestRunSTTPreparers_NoAudioFiles(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	o := newSTTOrchestrator(&sttExecutor{transcript: "hello"}, prep)

	imgFile := provider.MessageFile{MimeType: "image/png", Data: []byte{1, 2, 3}}
	content, files := o.runSTTPreparers(context.Background(), "original", []provider.MessageFile{imgFile})

	if content != "original" {
		t.Errorf("content = %q, want original", content)
	}
	if len(files) != 1 || files[0].MimeType != "image/png" {
		t.Errorf("files = %v, want original image file", files)
	}
}

func TestRunSTTPreparers_NoSTTPreparer(t *testing.T) {
	// Preparer exists but STT flag is false.
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: false}
	o := newSTTOrchestrator(&sttExecutor{transcript: "hello"}, prep)

	af := audioFile("audio/webm", []byte("audio-data"))
	content, files := o.runSTTPreparers(context.Background(), "original", []provider.MessageFile{af})

	if content != "original" {
		t.Errorf("content = %q, want original (no STT preparer)", content)
	}
	if len(files) != 1 {
		t.Errorf("files should be unchanged, got %v", files)
	}
}

func TestRunSTTPreparers_AudioTranscribed(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	o := newSTTOrchestrator(&sttExecutor{transcript: "book a meeting"}, prep)

	af := audioFile("audio/webm", []byte("audio-data"))
	content, files := o.runSTTPreparers(context.Background(), "", []provider.MessageFile{af})

	if content != "book a meeting" {
		t.Errorf("content = %q, want transcript", content)
	}
	if len(files) != 0 {
		t.Errorf("audio file should be removed after transcription, got %v", files)
	}
}

func TestRunSTTPreparers_TranscriptPrependedToExistingContent(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	o := newSTTOrchestrator(&sttExecutor{transcript: "hello world"}, prep)

	af := audioFile("audio/mp4", []byte("audio-data"))
	content, _ := o.runSTTPreparers(context.Background(), "extra context", []provider.MessageFile{af})

	if !strings.HasPrefix(content, "hello world") {
		t.Errorf("transcript should be prepended, got: %q", content)
	}
	if !strings.Contains(content, "extra context") {
		t.Errorf("original content should be preserved, got: %q", content)
	}
}

func TestRunSTTPreparers_NonAudioFilesPassThrough(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	o := newSTTOrchestrator(&sttExecutor{transcript: "hello"}, prep)

	af := audioFile("audio/ogg", []byte("audio"))
	img := provider.MessageFile{MimeType: "image/jpeg", Data: []byte{0xff}}
	content, files := o.runSTTPreparers(context.Background(), "", []provider.MessageFile{af, img})

	if content != "hello" {
		t.Errorf("content = %q, want transcript", content)
	}
	if len(files) != 1 || files[0].MimeType != "image/jpeg" {
		t.Errorf("only non-audio files should remain, got %v", files)
	}
}

func TestRunSTTPreparers_FailOpen_PassesAudioThrough(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true, FailOpen: true}
	o := newSTTOrchestrator(&sttExecutor{err: "whisper API error"}, prep)

	af := audioFile("audio/webm", []byte("audio"))
	content, files := o.runSTTPreparers(context.Background(), "original", []provider.MessageFile{af})

	if content != "original" {
		t.Errorf("content should be unchanged on fail_open error, got %q", content)
	}
	if len(files) != 1 || files[0].MimeType != "audio/webm" {
		t.Errorf("audio file should pass through on fail_open, got %v", files)
	}
}

func TestRunSTTPreparers_FailClosed_ReturnsOriginal(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true, FailOpen: false}
	o := newSTTOrchestrator(&sttExecutor{err: "whisper API error"}, prep)

	af := audioFile("audio/webm", []byte("audio"))
	content, files := o.runSTTPreparers(context.Background(), "original", []provider.MessageFile{af})

	if content != "original" {
		t.Errorf("content should be unchanged on fail-closed error, got %q", content)
	}
	if len(files) != 1 || files[0].MimeType != "audio/webm" {
		t.Errorf("original files should be returned on fail-closed, got %v", files)
	}
}

// --- runSTTPreparer: verify base64 args are passed correctly ---

func TestRunSTTPreparer_PassesBase64ArgsToPlugin(t *testing.T) {
	rawAudio := []byte("raw-audio-bytes")
	var receivedArgs map[string]string

	capturingExec := &capturingArgsExecutor{fn: func(call ToolCall) ToolResult {
		receivedArgs = call.Args
		return ToolResult{CallID: call.ID, Content: "transcript"}
	}}

	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:    "stt",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, capturingExec)

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	o := NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess, OrchestratorOpts{},
	)

	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	f := audioFile("audio/webm", rawAudio)
	transcript, err := o.runSTTPreparer(context.Background(), prep, f)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transcript != "transcript" {
		t.Errorf("transcript = %q, want transcript", transcript)
	}
	if receivedArgs["file_mime"] != "audio/webm" {
		t.Errorf("file_mime = %q, want audio/webm", receivedArgs["file_mime"])
	}
	decoded, err := base64.StdEncoding.DecodeString(receivedArgs["file_data"])
	if err != nil {
		t.Fatalf("file_data is not valid base64: %v", err)
	}
	if string(decoded) != string(rawAudio) {
		t.Errorf("decoded audio = %q, want %q", decoded, rawAudio)
	}
}

func TestRunSTTPreparers_MultiplePreparers_FirstSucceeds(t *testing.T) {
	// Two STT preparers, first succeeds — file should be transcribed once, not twice.
	reg := NewToolRegistry()
	exec1 := &sttExecutor{transcript: "hello from prep1"}
	exec2 := &sttExecutor{transcript: "hello from prep2"}
	_ = reg.Register(PluginCapability{
		Name:    "stt1",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, exec1)
	_ = reg.Register(PluginCapability{
		Name:    "stt2",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, exec2)

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	o := NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess,
		OrchestratorOpts{ContentPreparers: []ContentPreparerEntry{
			{Plugin: "stt1", Action: "transcribe", STT: true},
			{Plugin: "stt2", Action: "transcribe", STT: true},
		}},
	)

	af := audioFile("audio/webm", []byte("audio"))
	content, files := o.runSTTPreparers(context.Background(), "", []provider.MessageFile{af})

	if content != "hello from prep1" {
		t.Errorf("content = %q, want single transcript from first preparer", content)
	}
	if len(files) != 0 {
		t.Errorf("audio file should be consumed, got %d remaining", len(files))
	}
}

func TestRunSTTPreparers_MultiplePreparers_FirstFailsOpen(t *testing.T) {
	// First preparer fails (fail_open), second succeeds — file transcribed by second.
	reg := NewToolRegistry()
	exec1 := &sttExecutor{err: "fail"}
	exec2 := &sttExecutor{transcript: "hello from prep2"}
	_ = reg.Register(PluginCapability{
		Name:    "stt1",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, exec1)
	_ = reg.Register(PluginCapability{
		Name:    "stt2",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, exec2)

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	o := NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess,
		OrchestratorOpts{ContentPreparers: []ContentPreparerEntry{
			{Plugin: "stt1", Action: "transcribe", STT: true, FailOpen: true},
			{Plugin: "stt2", Action: "transcribe", STT: true},
		}},
	)

	af := audioFile("audio/webm", []byte("audio"))
	content, files := o.runSTTPreparers(context.Background(), "", []provider.MessageFile{af})

	if content != "hello from prep2" {
		t.Errorf("content = %q, want transcript from second preparer", content)
	}
	if len(files) != 0 {
		t.Errorf("audio file should be consumed, got %d remaining", len(files))
	}
}

func TestRunSTTPreparers_AllPreparersFail_FilePassedThrough(t *testing.T) {
	// Both preparers fail with fail_open — audio file should appear in remaining exactly once.
	reg := NewToolRegistry()
	exec1 := &sttExecutor{err: "fail1"}
	exec2 := &sttExecutor{err: "fail2"}
	_ = reg.Register(PluginCapability{
		Name:    "stt1",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, exec1)
	_ = reg.Register(PluginCapability{
		Name:    "stt2",
		Actions: []Action{{Name: "transcribe", Description: "transcribe"}},
	}, exec2)

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	o := NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess,
		OrchestratorOpts{ContentPreparers: []ContentPreparerEntry{
			{Plugin: "stt1", Action: "transcribe", STT: true, FailOpen: true},
			{Plugin: "stt2", Action: "transcribe", STT: true, FailOpen: true},
		}},
	)

	af := audioFile("audio/webm", []byte("audio"))
	content, files := o.runSTTPreparers(context.Background(), "original", []provider.MessageFile{af})

	if content != "original" {
		t.Errorf("content = %q, want original (all failed)", content)
	}
	if len(files) != 1 {
		t.Errorf("audio file should appear exactly once in remaining, got %d", len(files))
	}
}

func TestRunSTTPreparers_MultipleAudioFiles(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	o := newSTTOrchestrator(&sttExecutor{transcript: "transcribed"}, prep)

	af1 := audioFile("audio/webm", []byte("audio1"))
	af2 := audioFile("audio/mp4", []byte("audio2"))
	img := provider.MessageFile{MimeType: "image/png", Data: []byte{1}}
	content, files := o.runSTTPreparers(context.Background(), "", []provider.MessageFile{af1, af2, img})

	// Both audio files transcribed, each prepending "transcribed".
	if !strings.Contains(content, "transcribed") {
		t.Errorf("content should contain transcript, got %q", content)
	}
	// Only the image should remain.
	if len(files) != 1 || files[0].MimeType != "image/png" {
		t.Errorf("only non-audio files should remain, got %v", files)
	}
}

func TestRunSTTPreparer_UnknownPlugin(t *testing.T) {
	reg := NewToolRegistry()
	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	o := NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess, OrchestratorOpts{},
	)

	prep := ContentPreparerEntry{Plugin: "nonexistent", Action: "transcribe", STT: true}
	_, err := o.runSTTPreparer(context.Background(), prep, audioFile("audio/webm", []byte("x")))
	if err == nil {
		t.Error("expected error for unknown plugin/action")
	}
}

func TestRunSTTPreparer_FileTooLarge(t *testing.T) {
	prep := ContentPreparerEntry{Plugin: "stt", Action: "transcribe", STT: true}
	o := newSTTOrchestrator(&sttExecutor{transcript: "hello"}, prep)

	largeData := make([]byte, maxSTTFileSize+1)
	_, err := o.runSTTPreparer(context.Background(), prep, audioFile("audio/webm", largeData))
	if err == nil {
		t.Error("expected error for oversized audio file")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %q, want 'too large' message", err)
	}
}

type capturingArgsExecutor struct {
	fn func(ToolCall) ToolResult
}

func (e *capturingArgsExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return e.fn(call)
}

// --- end-to-end: audio in Run() is transcribed before LLM sees it ---

func TestRun_AudioFileTranscribedBeforeLLM(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:    "stt",
		Actions: []Action{{Name: "transcribe", Description: "transcribe audio"}},
	}, &sttExecutor{transcript: "remind me at 3pm"})

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	sess.Create("s-stt")

	llm := &capturingLLM{responses: []string{"ok"}}
	o := NewWithRules(llm, &fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess,
		OrchestratorOpts{
			ContentPreparers: []ContentPreparerEntry{
				{Plugin: "stt", Action: "transcribe", STT: true},
			},
		},
	)

	af := audioFile("audio/webm", []byte("audio"))
	if _, err := o.Run(context.Background(), "s-stt", "", af); err != nil {
		t.Fatal(err)
	}

	if len(llm.requests) == 0 {
		t.Fatal("LLM was never called")
	}
	// Find the user message in the LLM request.
	found := false
	for _, msg := range llm.requests[0].Messages {
		if strings.Contains(msg.Content, "remind me at 3pm") {
			found = true
		}
		// Audio file should not appear in LLM messages.
		if len(msg.Files) > 0 && strings.HasPrefix(msg.Files[0].MimeType, "audio/") {
			t.Error("audio file should not be sent to LLM after transcription")
		}
	}
	if !found {
		t.Error("transcript should appear in LLM messages")
	}
}
