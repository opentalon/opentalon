package plugin

import (
	"strings"
	"unicode/utf8"

	"github.com/opentalon/opentalon/proto/pluginpb"
)

// --- CapabilitiesMsg ---

// schemaToProto prepares a parameter's raw JSON Schema fragment for a proto3
// string field.
//
// Every other string in a capability arrives having been decoded by
// encoding/json into a Go string, which silently replaces invalid UTF-8 with
// U+FFFD on the way. A json.RawMessage does not: it holds whatever bytes the
// source contained. A plugin that bridges a third-party tool server does not
// control those bytes, and proto3 refuses to marshal a string field that is
// not valid UTF-8 — so one bad byte in one property of one tool would fail
// the whole Capabilities call and register none of the plugin's tools.
//
// Replacing rather than rejecting keeps the fragment a valid schema: U+FFFD
// is legal inside a JSON string. Same treatment, and the same reason, as
// ensureValidUTF8Map on the argument path.
func schemaToProto(schema []byte) string {
	if utf8.Valid(schema) {
		return string(schema)
	}
	return strings.ToValidUTF8(string(schema), "�")
}

func capsToProto(c CapabilitiesMsg) *pluginpb.PluginCapabilities {
	actions := make([]*pluginpb.Action, len(c.Actions))
	for i, a := range c.Actions {
		params := make([]*pluginpb.Parameter, len(a.Parameters))
		for j, p := range a.Parameters {
			params[j] = &pluginpb.Parameter{
				Name:        p.Name,
				Description: p.Description,
				Type:        p.Type,
				Required:    p.Required,
				Schema:      schemaToProto(p.Schema),
			}
		}
		actions[i] = &pluginpb.Action{
			Name:              a.Name,
			Description:       a.Description,
			Parameters:        params,
			UserOnly:          a.UserOnly,
			InjectContextArgs: a.InjectContextArgs,
			AlwaysInclude:     a.AlwaysInclude,
			ReadOnly:          a.ReadOnly,
		}
	}
	glossary := make([]*pluginpb.GlossaryEntry, len(c.Glossary))
	for i, g := range c.Glossary {
		glossary[i] = &pluginpb.GlossaryEntry{
			Term:       g.Term,
			Definition: g.Definition,
			Category:   g.Category,
			Tags:       g.Tags,
			Synonyms:   g.Synonyms,
		}
	}
	knowledge := make([]*pluginpb.KnowledgeArticle, len(c.KnowledgeArticles))
	for i, k := range c.KnowledgeArticles {
		knowledge[i] = &pluginpb.KnowledgeArticle{
			Id:      k.ID,
			Title:   k.Title,
			Content: k.Content,
			Tags:    k.Tags,
		}
	}
	return &pluginpb.PluginCapabilities{
		Name:                 c.Name,
		Description:          c.Description,
		Actions:              actions,
		SystemPromptAddition: c.SystemPromptAddition,
		Glossary:             glossary,
		KnowledgeArticles:    knowledge,
		SupportsCallbacks:    c.SupportsCallbacks,
	}
}

// --- Request / Response ---

func requestFromProto(pb *pluginpb.ToolCallRequest) Request {
	if pb == nil {
		return Request{}
	}
	var creds map[string]CredentialHeader
	if len(pb.CredentialHeaders) > 0 {
		creds = make(map[string]CredentialHeader, len(pb.CredentialHeaders))
		for k, v := range pb.CredentialHeaders {
			creds[k] = CredentialHeader{Header: v.Header, Value: v.Value}
		}
	}
	return Request{
		Method:            "execute",
		ID:                pb.Id,
		Plugin:            pb.Plugin,
		Action:            pb.Action,
		Args:              pb.Args,
		CredentialHeaders: creds,
	}
}

func responseToProto(r Response) *pluginpb.ToolResultResponse {
	return &pluginpb.ToolResultResponse{
		CallId:            r.CallID,
		Content:           r.Content,
		StructuredContent: r.StructuredContent,
		Error:             r.Error,
	}
}
