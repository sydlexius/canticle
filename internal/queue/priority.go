package queue

const (
	// PriorityMiss is the priority applied to deferred benign-miss rows.
	// Negative values sink below all fresh work in the ORDER BY priority DESC
	// sort so re-attempts from an escalating miss cadence are always processed
	// after new or normal-priority items.
	PriorityMiss = -100
	// PriorityScan is the baseline priority used for library scan-enqueued work.
	PriorityScan = 0
	// PriorityWebhook is used for webhook-enqueued work that should jump ahead of scan backlog.
	PriorityWebhook = 10
)
