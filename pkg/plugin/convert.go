package plugin

import (
	"github.com/opentalon/opentalon/proto/pluginpb"
)

// --- CapabilitiesMsg ---

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
			}
		}
		actions[i] = &pluginpb.Action{
			Name:              a.Name,
			Description:       a.Description,
			Parameters:        params,
			UserOnly:          a.UserOnly,
			InjectContextArgs: a.InjectContextArgs,
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
	return &pluginpb.PluginCapabilities{
		Name:                 c.Name,
		Description:          c.Description,
		Actions:              actions,
		SystemPromptAddition: c.SystemPromptAddition,
		Glossary:             glossary,
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
