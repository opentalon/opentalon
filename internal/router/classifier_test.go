package router

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func TestClassifyCode(t *testing.T) {
	c := NewTaskClassifier()
	tests := []struct {
		name    string
		content string
	}{
		{"code block", "Please fix this:\n```go\nfunc main() {}\n```"},
		{"func keyword", "Write func handleRequest(w http.ResponseWriter)"},
		{"def keyword", "def process_data(items):"},
		{"package keyword", "package main"},
		{"import keyword", "import os"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := []provider.Message{{Role: provider.RoleUser, Content: tt.content}}
			if got := c.Classify(msgs); got != TaskCode {
				t.Errorf("Classify() = %q, want code", got)
			}
		})
	}
}

func TestClassifyChat(t *testing.T) {
	c := NewTaskClassifier()
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "What's the weather?"}}
	if got := c.Classify(msgs); got != TaskChat {
		t.Errorf("Classify() = %q, want chat", got)
	}
}

func TestClassifyTransform(t *testing.T) {
	c := NewTaskClassifier()
	tests := []struct {
		name    string
		content string
	}{
		{"translate", "Translate this to French"},
		{"summarize", "Please summarize this article"},
		{"convert", "Convert this CSV to JSON"},
		{"rewrite", "Rewrite this paragraph"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := []provider.Message{{Role: provider.RoleUser, Content: tt.content}}
			if got := c.Classify(msgs); got != TaskTransform {
				t.Errorf("Classify() = %q, want transform", got)
			}
		})
	}
}

func TestClassifyAnalysis(t *testing.T) {
	c := NewTaskClassifier()
	longContent := strings.Repeat("This is a detailed analysis request. ", 20)
	msgs := []provider.Message{{Role: provider.RoleUser, Content: longContent}}
	if got := c.Classify(msgs); got != TaskAnalysis {
		t.Errorf("Classify() = %q, want analysis", got)
	}
}

func TestClassifyDeepConversation(t *testing.T) {
	c := NewTaskClassifier()
	msgs := make([]provider.Message, 15)
	for i := range msgs {
		msgs[i] = provider.Message{Role: provider.RoleUser, Content: "message"}
	}
	if got := c.Classify(msgs); got != TaskDeepConversation {
		t.Errorf("Classify() = %q, want deep_conversation", got)
	}
}

func TestClassifyEmpty(t *testing.T) {
	c := NewTaskClassifier()
	if got := c.Classify(nil); got != TaskGeneral {
		t.Errorf("Classify(nil) = %q, want general", got)
	}
}

func TestClassifyGeneral(t *testing.T) {
	c := NewTaskClassifier()
	content := "Explain the differences between TCP and UDP protocols\nin networking and when to use each one"
	msgs := []provider.Message{{Role: provider.RoleUser, Content: content}}
	if got := c.Classify(msgs); got != TaskGeneral {
		t.Errorf("Classify() = %q, want general", got)
	}
}
