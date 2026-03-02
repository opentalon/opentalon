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
			Name:        a.Name,
			Description: a.Description,
			Parameters:  params,
		}
	}
	return &pluginpb.PluginCapabilities{
		Name:        c.Name,
		Description: c.Description,
		Actions:     actions,
	}
}

func capsFromProto(pb *pluginpb.PluginCapabilities) *CapabilitiesMsg {
	if pb == nil {
		return nil
	}
	actions := make([]ActionMsg, len(pb.Actions))
	for i, a := range pb.Actions {
		params := make([]ParameterMsg, len(a.Parameters))
		for j, p := range a.Parameters {
			params[j] = ParameterMsg{
				Name:        p.Name,
				Description: p.Description,
				Type:        p.Type,
				Required:    p.Required,
			}
		}
		actions[i] = ActionMsg{
			Name:        a.Name,
			Description: a.Description,
			Parameters:  params,
		}
	}
	return &CapabilitiesMsg{
		Name:        pb.Name,
		Description: pb.Description,
		Actions:     actions,
	}
}

// --- Request / Response ---

func requestToProto(r Request) *pluginpb.ToolCallRequest {
	return &pluginpb.ToolCallRequest{
		Id:     r.ID,
		Plugin: r.Plugin,
		Action: r.Action,
		Args:   r.Args,
	}
}

func requestFromProto(pb *pluginpb.ToolCallRequest) Request {
	if pb == nil {
		return Request{}
	}
	return Request{
		Method: "execute",
		ID:     pb.Id,
		Plugin: pb.Plugin,
		Action: pb.Action,
		Args:   pb.Args,
	}
}

func responseToProto(r Response) *pluginpb.ToolResultResponse {
	return &pluginpb.ToolResultResponse{
		CallId:  r.CallID,
		Content: r.Content,
		Error:   r.Error,
	}
}

func responseFromProto(pb *pluginpb.ToolResultResponse) Response {
	if pb == nil {
		return Response{}
	}
	return Response{
		CallID:  pb.CallId,
		Content: pb.Content,
		Error:   pb.Error,
	}
}
