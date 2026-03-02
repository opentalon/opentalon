package channel

import (
	"github.com/opentalon/opentalon/pkg/channel/channelpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- Capabilities ---

func capabilitiesToProto(c Capabilities) *channelpb.ChannelCapabilities {
	return &channelpb.ChannelCapabilities{
		Id:               c.ID,
		Name:             c.Name,
		Threads:          c.Threads,
		Files:            c.Files,
		Reactions:        c.Reactions,
		Edits:            c.Edits,
		MaxMessageLength: c.MaxMessageLength,
	}
}

func capabilitiesFromProto(pb *channelpb.ChannelCapabilities) Capabilities {
	if pb == nil {
		return Capabilities{}
	}
	return Capabilities{
		ID:               pb.Id,
		Name:             pb.Name,
		Threads:          pb.Threads,
		Files:            pb.Files,
		Reactions:        pb.Reactions,
		Edits:            pb.Edits,
		MaxMessageLength: pb.MaxMessageLength,
	}
}

// --- InboundMessage ---

func inboundToProto(m InboundMessage) *channelpb.InboundMessage {
	pb := &channelpb.InboundMessage{
		ChannelId:      m.ChannelID,
		ConversationId: m.ConversationID,
		ThreadId:       m.ThreadID,
		SenderId:       m.SenderID,
		SenderName:     m.SenderName,
		Content:        m.Content,
		Metadata:       m.Metadata,
		Timestamp:      timestamppb.New(m.Timestamp),
	}
	for _, f := range m.Files {
		pb.Files = append(pb.Files, fileToProto(f))
	}
	return pb
}

func inboundFromProto(pb *channelpb.InboundMessage) InboundMessage {
	if pb == nil {
		return InboundMessage{}
	}
	m := InboundMessage{
		ChannelID:      pb.ChannelId,
		ConversationID: pb.ConversationId,
		ThreadID:       pb.ThreadId,
		SenderID:       pb.SenderId,
		SenderName:     pb.SenderName,
		Content:        pb.Content,
		Metadata:       pb.Metadata,
	}
	if pb.Timestamp != nil {
		m.Timestamp = pb.Timestamp.AsTime()
	}
	for _, f := range pb.Files {
		m.Files = append(m.Files, fileFromProto(f))
	}
	return m
}

// --- OutboundMessage ---

func outboundToProto(m OutboundMessage) *channelpb.OutboundMessage {
	pb := &channelpb.OutboundMessage{
		ConversationId: m.ConversationID,
		ThreadId:       m.ThreadID,
		Content:        m.Content,
		Metadata:       m.Metadata,
	}
	for _, f := range m.Files {
		pb.Files = append(pb.Files, fileToProto(f))
	}
	return pb
}

func outboundFromProto(pb *channelpb.OutboundMessage) OutboundMessage {
	if pb == nil {
		return OutboundMessage{}
	}
	m := OutboundMessage{
		ConversationID: pb.ConversationId,
		ThreadID:       pb.ThreadId,
		Content:        pb.Content,
		Metadata:       pb.Metadata,
	}
	for _, f := range pb.Files {
		m.Files = append(m.Files, fileFromProto(f))
	}
	return m
}

// --- FileAttachment ---

func fileToProto(f FileAttachment) *channelpb.FileAttachment {
	return &channelpb.FileAttachment{
		Name:     f.Name,
		MimeType: f.MimeType,
		Data:     f.Data,
		Size:     f.Size,
	}
}

func fileFromProto(pb *channelpb.FileAttachment) FileAttachment {
	if pb == nil {
		return FileAttachment{}
	}
	return FileAttachment{
		Name:     pb.Name,
		MimeType: pb.MimeType,
		Data:     pb.Data,
		Size:     pb.Size,
	}
}

// --- ToolDefinition ---

func toolsToProto(tools []ToolDefinition) []*channelpb.ToolDefinition {
	out := make([]*channelpb.ToolDefinition, len(tools))
	for i, t := range tools {
		params := make([]*channelpb.ToolParam, len(t.Parameters))
		for j, p := range t.Parameters {
			params[j] = &channelpb.ToolParam{
				Name:        p.Name,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		out[i] = &channelpb.ToolDefinition{
			Plugin:            t.Plugin,
			Description:       t.Description,
			Action:            t.Action,
			ActionDescription: t.ActionDesc,
			Method:            t.Method,
			Url:               t.URL,
			Body:              t.Body,
			Headers:           t.Headers,
			RequiredEnv:       t.RequiredEnv,
			Parameters:        params,
		}
	}
	return out
}

func toolsFromProto(pbs []*channelpb.ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, len(pbs))
	for i, pb := range pbs {
		params := make([]ToolParam, len(pb.Parameters))
		for j, p := range pb.Parameters {
			params[j] = ToolParam{
				Name:        p.Name,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		out[i] = ToolDefinition{
			Plugin:      pb.Plugin,
			Description: pb.Description,
			Action:      pb.Action,
			ActionDesc:  pb.ActionDescription,
			Method:      pb.Method,
			URL:         pb.Url,
			Body:        pb.Body,
			Headers:     pb.Headers,
			RequiredEnv: pb.RequiredEnv,
			Parameters:  params,
		}
	}
	return out
}

// --- Config helpers ---

func configToStruct(cfg map[string]interface{}) *structpb.Struct {
	if cfg == nil {
		return nil
	}
	s, err := structpb.NewStruct(cfg)
	if err != nil {
		return nil
	}
	return s
}

func configFromStruct(s *structpb.Struct) map[string]interface{} {
	if s == nil {
		return nil
	}
	return s.AsMap()
}
