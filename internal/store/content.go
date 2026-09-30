package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// AttachmentData is one attachment part decoded from a hydrated
// message, ready for the blob store.
type AttachmentData struct {
	PartID    string
	MediaType string
	Name      string
	Data      []byte
}

// BodyResult is what convert produces from a fetched RFC 5322 body
// (PLAN §5 hydration).
type BodyResult struct {
	// Values maps partId → decoded body text for body parts.
	Values map[string]string
	// Attachments are written to the blob store and their blobIds
	// injected into the structure.
	Attachments []AttachmentData
	// Preview is the plain-text preview derived from the body; empty
	// means "leave the stored preview alone".
	Preview string
}

// PutHydrated stores one message's hydration result: attachment files +
// rows, body values, and the structure with blobIds injected. It sets
// hydrated_at and bumps the Email state only when a summary field the
// client can already see (the preview) actually changes (PLAN §5) —
// hydration itself must not make every connected client refetch.
func (s *Store) PutHydrated(ctx context.Context, account, emailID string, res BodyResult) error {
	var oldPreview, structure, subjectL, senderL, recipientL string
	err := s.db.QueryRowContext(ctx,
		`SELECT e.preview, c.body_structure, c.subject_l, c.from_l, c.to_l FROM emails e
		 JOIN email_content c ON c.id = e.id WHERE e.id = ? AND e.account = ?`,
		emailID, account).Scan(&oldPreview, &structure, &subjectL, &senderL, &recipientL)
	if err == sql.ErrNoRows {
		return fmt.Errorf("store: hydrate: email %s not found", emailID)
	}
	if err != nil {
		return fmt.Errorf("store: hydrate lookup: %w", err)
	}
	preview := oldPreview
	if res.Preview != "" {
		preview = res.Preview
	}
	previewChanged := preview != oldPreview

	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		var seq int64
		// Hydration always rewrites the index row, so the counter is
		// needed on every path; only the preview change also touches
		// the Email state with it.
		seq, err = nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if len(res.Attachments) > 0 {
			structure, err = injectBlobIDs(ctx, tx, s.blobs, account, structure, res.Attachments)
			if err != nil {
				return err
			}
		}
		values, err := jsonString(res.Values)
		if err != nil {
			return err
		}
		if values == "" || values == "null" {
			values = "{}"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE email_content SET body_values = ?, body_structure = ?, hydrated_at = ?
			 WHERE id = ?`,
			values, structure, time.Now().UnixMicro(), emailID); err != nil {
			return fmt.Errorf("store: write body: %w", err)
		}
		// Body tokens enter the index now (FR-X.5). The bump moves
		// queryState so text-search clients pick up late matches; the
		// Email state stays put — hydrated bodies are not a refetch
		// event for clients holding summaries (PLAN §5).
		if err := ftsReplace(ctx, tx, emailID, subjectL, senderL, recipientL,
			indexBodyText(structure, res.Values)); err != nil {
			return err
		}
		if err := bumpFTSState(ctx, tx, account, seq); err != nil {
			return err
		}
		if previewChanged {
			if _, err := tx.ExecContext(ctx,
				`UPDATE emails SET preview = ?, updated_modseq = ? WHERE id = ? AND account = ?`,
				preview, seq, emailID, account); err != nil {
				return fmt.Errorf("store: write preview: %w", err)
			}
			return bumpEmailState(ctx, tx, account, seq)
		}
		return nil
	})
}

// SetPreviews stores lazy preview fetches (PLAN §5). Like PutHydrated
// it bumps state only for previews that are actually new.
func (s *Store) SetPreviews(ctx context.Context, account string, previews map[string]string) error {
	if len(previews) == 0 {
		return nil
	}
	changed := map[string]string{}
	for id, prev := range previews {
		if prev == "" {
			continue
		}
		var cur string
		err := s.db.QueryRowContext(ctx,
			`SELECT preview FROM emails WHERE id = ? AND account = ?`, id, account).Scan(&cur)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		if cur == "" {
			changed[id] = prev
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		for id, prev := range changed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE emails SET preview = ?, updated_modseq = ? WHERE id = ? AND account = ?`,
				prev, seq, id, account); err != nil {
				return err
			}
		}
		return bumpEmailState(ctx, tx, account, seq)
	})
}

// injectBlobIDs writes each attachment to the blob store and stamps its
// blobId into the structure tree, returning the new structure JSON.
func injectBlobIDs(ctx context.Context, tx *sql.Tx, blobs *BlobStore, account, structure string, atts []AttachmentData) (string, error) {
	var tree map[string]any
	if err := json.Unmarshal([]byte(structure), &tree); err != nil {
		return "", fmt.Errorf("store: decode structure: %w", err)
	}
	byPart := map[string]AttachmentData{}
	for _, a := range atts {
		byPart[a.PartID] = a
	}
	blobIDs := map[string]string{}
	for _, a := range atts {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return "", err
		}
		id := newID(time.Now(), seq)
		if err := blobs.put(ctx, tx, account, id, a.MediaType, a.Data); err != nil {
			return "", err
		}
		blobIDs[a.PartID] = id
	}
	setBlobIDs(tree, blobIDs)
	out, err := json.Marshal(tree)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// setBlobIDs walks the EmailBodyPart tree stamping blobIds by partId.
func setBlobIDs(node map[string]any, blobIDs map[string]string) {
	if pid, ok := node["partId"].(string); ok {
		if id, ok := blobIDs[pid]; ok {
			node["blobId"] = id
		}
	}
	if subs, ok := node["subParts"].([]any); ok {
		for _, s := range subs {
			if child, ok := s.(map[string]any); ok {
				setBlobIDs(child, blobIDs)
			}
		}
	}
}

// --- structure analysis (FR-M.4) ---

type bodyPart struct {
	PartID      string          `json:"partId"`
	Type        string          `json:"type"`
	Size        int64           `json:"size"`
	Name        string          `json:"name,omitempty"`
	Disposition string          `json:"disposition,omitempty"`
	BlobID      string          `json:"blobId,omitempty"`
	SubParts    json.RawMessage `json:"subParts,omitempty"`
}

// analyzeStructure derives the textBody/htmlBody selections and the
// attachment list from a stored EmailBodyPart tree, using the same
// classification convert does (attachments are parts that are not body
// representations; RFC 8621 §4.1.3) so Email/get and the backfill
// summary can never disagree about hasAttachment.
func analyzeStructure(raw json.RawMessage) (textParts, htmlParts []string, attachments []jmapapi.Attachment) {
	var root bodyPart
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, nil
	}
	var walk func(p bodyPart)
	walk = func(p bodyPart) {
		media := strings.ToLower(p.Type)
		if strings.HasPrefix(media, "multipart/") || len(p.SubParts) > 0 && media == "" {
			for _, s := range p.subParts() {
				walk(s)
			}
			return
		}
		if strings.EqualFold(p.Disposition, "attachment") ||
			(media != "text/plain" && media != "text/html" && !strings.HasPrefix(media, "multipart/")) {
			attachments = append(attachments, jmapapi.Attachment{
				PartID:      p.PartID,
				BlobID:      p.BlobID,
				Type:        p.Type,
				Size:        p.Size,
				Name:        p.Name,
				Disposition: p.Disposition,
			})
			return
		}
		if p.PartID == "" {
			return
		}
		switch media {
		case "text/plain":
			textParts = append(textParts, p.PartID)
		case "text/html":
			htmlParts = append(htmlParts, p.PartID)
		}
	}
	walk(root)
	return textParts, htmlParts, attachments
}

// subParts decodes the child list of a container part.
func (p bodyPart) subParts() []bodyPart {
	if len(p.SubParts) == 0 {
		return nil
	}
	var subs []bodyPart
	if err := json.Unmarshal(p.SubParts, &subs); err != nil {
		return nil
	}
	return subs
}
