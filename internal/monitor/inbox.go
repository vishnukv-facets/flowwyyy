package monitor

import "flow/internal/inbox"

type InboxEventMeta = inbox.InboxEventMeta
type InboxEntry = inbox.InboxEntry

var (
	TaskDir                     = inbox.TaskDir
	InboxPath                   = inbox.InboxPath
	CursorPath                  = inbox.CursorPath
	MonitorCursorPath           = inbox.MonitorCursorPath
	AppendInboxEvent            = inbox.AppendInboxEvent
	AppendInboxEventStamped     = inbox.AppendInboxEventStamped
	AppendFlowTellEvent         = inbox.AppendFlowTellEvent
	ClassifyInboxEvent          = inbox.ClassifyInboxEvent
	FlowTellEvent               = inbox.FlowTellEvent
	ReadInboxEntries            = inbox.ReadInboxEntries
	ReadInboxMonitorCursor      = inbox.ReadInboxMonitorCursor
	WriteInboxMonitorCursor     = inbox.WriteInboxMonitorCursor
	SeedInboxMonitorCursorToEnd = inbox.SeedInboxMonitorCursorToEnd
	ReadInboxCursor             = inbox.ReadInboxCursor
	WriteInboxCursor            = inbox.WriteInboxCursor
)
