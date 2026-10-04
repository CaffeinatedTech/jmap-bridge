package gmailapi

import "testing"

// TestCostTable pins the quota table verified against Google's docs 2026-10-03
// (the M9 gate). If any of these change, that is a deliberate re-measurement and
// the plan's §4.3 table must move in the same commit.
func TestCostTable(t *testing.T) {
	want := map[string]int{
		"getProfile":           CostGetProfile,
		"labels.list":          CostLabelsList,
		"labels.create":        CostLabelsCreate,
		"labels.update":        CostLabelsUpdate,
		"labels.delete":        CostLabelsDelete,
		"history.list":         CostHistoryList,
		"messages.list":        CostMessagesList,
		"messages.get":         CostMessagesGet,
		"messages.import":      CostMessagesImport,
		"messages.modify":      CostMessagesModify,
		"messages.delete":      CostMessagesDelete,
		"messages.batchDelete": CostMessagesBatchDelete,
		"messages.batchModify": CostMessagesBatchModify,
		"attachments.get":      CostAttachmentsGet,
		"messages.send":        CostMessagesSend,
		"threads.get":          CostThreadsGet,
		"drafts.create":        CostDraftsCreate,
		"drafts.get":           CostDraftsGet,
		"drafts.send":          CostDraftsSend,
		"watch":                CostWatch,
		"stop":                 CostStop,
	}
	values := map[string]int{
		"getProfile":           1,
		"labels.list":          1,
		"labels.create":        5,
		"labels.update":        5,
		"labels.delete":        5,
		"history.list":         2,
		"messages.list":        5,
		"messages.get":         20,
		"messages.import":      25,
		"messages.modify":      5,
		"messages.delete":      10,
		"messages.batchDelete": 50,
		"messages.batchModify": 50,
		"attachments.get":      20,
		"messages.send":        100,
		"threads.get":          40,
		"drafts.create":        10,
		"drafts.get":           20,
		"drafts.send":          100,
		"watch":                100,
		"stop":                 50,
	}
	for name, got := range want {
		if got != values[name] {
			t.Errorf("%s = %d, want %d (re-verify against Google's quota docs)", name, got, values[name])
		}
	}
	if DefaultQuotaUnitsPerSecond != 100 {
		t.Errorf("DefaultQuotaUnitsPerSecond = %d, want 100 (6000/min per-user budget)", DefaultQuotaUnitsPerSecond)
	}
}
