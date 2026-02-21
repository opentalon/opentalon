package router

type TaskType string

const (
	TaskCode             TaskType = "code"
	TaskChat             TaskType = "chat"
	TaskAnalysis         TaskType = "analysis"
	TaskTransform        TaskType = "transform"
	TaskDeepConversation TaskType = "deep_conversation"
	TaskGeneral          TaskType = "general"
)
