package gmailapi

// Per-method Gmail API quota costs, in quota units. Verified 2026-10-03 against
// Google's published per-method usage table
// (https://developers.google.com/workspace/gmail/api/reference/quota#per-method-quota).
// The numbers had moved since GMAIL_API_PLAN §4.3 was drafted — most notably
// messages.get is 20, not 5 — so the M9 re-measurement corrected the plan. This
// table is the single source of truth for the pacer and the batch accounting;
// update it when Google re-measures.
const (
	CostGetProfile          = 1
	CostLabelsList          = 1
	CostLabelsGet           = 1
	CostLabelsCreate        = 5
	CostLabelsUpdate        = 5
	CostLabelsDelete        = 5
	CostHistoryList         = 2
	CostMessagesList        = 5
	CostMessagesGet         = 20
	CostMessagesImport      = 25
	CostMessagesModify      = 5
	CostMessagesDelete      = 10
	CostMessagesBatchDelete = 50
	CostMessagesBatchModify = 50
	CostAttachmentsGet      = 20
	CostMessagesSend        = 100
	CostThreadsGet          = 40
	CostDraftsCreate        = 10
	CostDraftsGet           = 20
	CostDraftsSend          = 100
	CostDraftsDelete        = 10
	CostWatch               = 100
	CostStop                = 50
)

// DefaultQuotaUnitsPerSecond paces API calls when a quota is not configured.
// Google's current per-minute per-user budget is 6 000 units
// (https://developers.google.com/workspace/gmail/api/reference/quota, verified
// 2026-10-03), i.e. 100 units/s; pacing at that rate keeps a single account
// just inside the budget rather than at the 200 the plan first assumed. The
// value is deliberately conservative and configurable per account (M10).
const DefaultQuotaUnitsPerSecond = 100

// DefaultMaxRetries bounds automatic retries of a retryable call (throttle or
// transient transport/5xx). It is attempts after the first, so a call is made
// at most DefaultMaxRetries+1 times.
const DefaultMaxRetries = 4
