package imapdrv

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// Folder is one mailbox as discovery reports it — hierarchy and
// SPECIAL-USE role already decoded into the JMAP vocabulary (FR-S.1).
type Folder struct {
	// Name is the full mailbox path exactly as the server reports it,
	// separator and namespace prefix included (FR-M.1).
	Name string
	// Delim is the hierarchy separator inside Name.
	Delim rune
	// Role is the JMAP role derived from SPECIAL-USE (or well-known
	// names when the server offers none), "" when unrole'd.
	Role string
	// NoSelect marks a hierarchy container (RFC 3501 \Noselect).
	NoSelect bool
	// AllMail marks the \All mailbox (Gmail's [Gmail]/All Mail): its
	// membership is implicit, so clients cannot add to it or remove
	// from it (FR-S.10, FR-M.18).
	AllMail bool
}

// ListFolders returns every mailbox via LIST-EXTENDED with the
// SPECIAL-USE return option (FR-S.1). "*" matches through hierarchy
// separators, so one round trip covers the whole tree. The command form
// (not the convenience wrapper) keeps the tagged response code visible:
// a [THROTTLED] LIST must be an error — feeding a partial list to the
// store's reconciliation would tombstone every folder the throttle
// happened to omit.
func (c *Conn) ListFolders(ctx context.Context) ([]Folder, error) {
	data, err := waitCmd(ctx, c.client.List("", "*", &imapclient.ListOptions{
		ReturnOptions: []imapclient.ListReturnOption{imapclient.ListReturnSpecialUse},
	}))
	if err != nil {
		if IsThrottled(err) {
			return nil, ErrThrottled
		}
		return nil, fmt.Errorf("imapdrv: list: %w", err)
	}
	out := make([]Folder, 0, len(data))
	for _, d := range data {
		f := Folder{Name: d.Mailbox, Delim: d.Delimiter}
		for _, a := range d.Attrs {
			switch a {
			case imap.MailboxAttrNoSelect, imap.MailboxAttrNonExistent:
				// \NonExistent is IMAP4rev2's name for what rev1 called
				// \Noselect; Gmail's "[Gmail]" container reports it.
				f.NoSelect = true
			case imap.MailboxAttrAll:
				f.AllMail = true
			}
		}
		f.Role = roleFor(f.Name, d.Attrs, c.GmailExt())
		out = append(out, f)
	}
	return out, nil
}

// roleFor maps SPECIAL-USE attributes (RFC 6154) to JMAP roles, with a
// well-known-name fallback for servers that implement SPECIAL-USE only
// by convention (FR-S.1, FR-M.1). On an X-GM-EXT-1 server the fallback
// is off: Gmail labels named "Sent" or "Trash" are ordinary labels, and
// granting them roles would collide with the SPECIAL-USE system folders
// ([Gmail]/Sent Mail, [Gmail]/Bin) and make role lookups ambiguous.
func roleFor(name string, attrs []imap.MailboxAttr, gmail bool) string {
	for _, a := range attrs {
		switch a {
		case imap.MailboxAttrSent:
			return "sent"
		case imap.MailboxAttrDrafts:
			return "drafts"
		case imap.MailboxAttrTrash:
			return "trash"
		case imap.MailboxAttrJunk:
			return "junk"
		case imap.MailboxAttrArchive:
			return "archive"
		case imap.MailboxAttrAll:
			// \All ([Gmail]/All Mail on Gmail): JMAP has no matching
			// role, and the archive semantics REQUIREMENTS FR-M.18
			// prescribes — "archive removes from INBOX only; the message
			// remains listed in All Mail" — need an archive-role mailbox
			// that already holds everything. All Mail is that mailbox
			// (decided 2026-09-29 with the user); its membership is
			// implicit, which the sync engine's write path enforces.
			return "archive"
		}
	}
	if strings.EqualFold(name, "INBOX") {
		return "inbox"
	}
	if gmail {
		// Gmail: labels are labels; only SPECIAL-USE and INBOX name
		// carry roles (FR-S.10's label namespace).
		return ""
	}
	// Well-known top-level names (no SPECIAL-USE on many cPanel boxes).
	base := name
	if i := strings.LastIndexAny(name, "/."); i >= 0 {
		if i+1 < len(name) && strings.Count(name, "/")+strings.Count(name, ".") == 1 {
			base = name[i+1:]
		} else if !strings.Contains(name[i+1:], "/") && !strings.Contains(name[i+1:], ".") {
			base = name[i+1:]
		} else {
			return ""
		}
	}
	switch strings.ToLower(base) {
	case "sent", "sent items", "sent messages":
		return "sent"
	case "drafts", "draft":
		return "drafts"
	case "trash", "deleted", "deleted items":
		return "trash"
	case "junk", "spam", "bulk mail":
		return "junk"
	case "archive", "archives":
		return "archive"
	default:
		return ""
	}
}

// Namespace reads NAMESPACE (RFC 2342) for the personal prefix and
// separator, logging the result per FR-S.1. Both values are advisory:
// LIST already reports the separator per mailbox, and names are stored
// exactly as LIST returns them.
func (c *Conn) Namespace(ctx context.Context) (prefix string, delim rune, err error) {
	data, err := waitCmd(ctx, c.client.Namespace(nil))
	if err != nil {
		return "", 0, fmt.Errorf("imapdrv: namespace: %w", err)
	}
	desc := firstNamespace(data)
	if desc == nil {
		return "", 0, nil
	}
	return desc.Prefix, desc.Delimiter, nil
}

func firstNamespace(data *imapclient.NamespaceData) *imapclient.NamespaceDescriptor {
	if data == nil {
		return nil
	}
	for _, group := range [][]imapclient.NamespaceDescriptor{data.Personal, data.Shared, data.OtherUsers} {
		if len(group) > 0 {
			return &group[0]
		}
	}
	return nil
}

// FolderStatus is the STATUS view of one folder (FR-S.3 anchors).
type FolderStatus struct {
	UIDValidity   uint32
	UIDNext       uint64
	HighestModSeq uint64
	Messages      uint32
	Unseen        uint32
}

// Status reads one folder's status without selecting it.
func (c *Conn) Status(ctx context.Context, folder string) (FolderStatus, error) {
	st, err := waitCmd(ctx, c.client.Status(folder, &imapclient.StatusOptions{
		Items: []imap.StatusItem{
			imap.StatusItemMessages, imap.StatusItemUIDNext, imap.StatusItemUIDValidity,
			imap.StatusItemUnseen, imap.StatusItemHighestModSeq,
		},
	}))
	if err != nil {
		return FolderStatus{}, fmt.Errorf("imapdrv: status %q: %w", folder, err)
	}
	data := st
	return FolderStatus{
		UIDValidity:   data.UIDValidity,
		UIDNext:       uint64(data.UIDNext),
		HighestModSeq: data.HighestModSeq,
		Messages:      data.NumMessages,
		Unseen:        data.NumUnseen,
	}, nil
}
