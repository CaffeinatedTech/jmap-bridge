package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// This file is the blob surface (PLAN §7.3, FR-M.16 and FR-M.17): one
// upload endpoint for every blob an account may reference, one download
// endpoint that serves them back, both behind the same Basic auth as
// everything else (FR-J.6), both on the session's origin (FR-J.10), and
// both advertised as the RFC 8620 §2 URI templates the session carries.

// handleUpload serves POST /{account}/upload/ (RFC 8620 §6.1): raw
// bytes in, {accountId, blobId, type, size} out. The account comes from
// the path prefix the session's {accountId} expands into, so a blob is
// filed under exactly the account that uploaded it (FR-M.17, FR-A.11).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	acct := s.authorize(w, r, r.PathValue("account"))
	if acct == nil {
		if s.cfg.Auth.Mode == "none" {
			http.NotFound(w, r)
		}
		return
	}
	if r.ContentLength > maxSizeUpload {
		writeLimit(w, "upload body exceeds maxSizeUpload")
		return
	}
	mediaType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if mediaType == "" {
		// The bytes arrive unlabelled; the session's default is the
		// type RFC 8620 §6.1 expects a client to have sent.
		mediaType = "application/octet-stream"
	}
	// "Unsupported" content is content the server cannot even name:
	// RFC 6838 requires a type/subtype, and a type we could not parse
	// is one we could not answer with either.
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:jmap:error:invalidArguments",
			"Content-Type is not a valid media type")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxSizeUpload+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:jmap:error:invalidArguments",
			"could not read the upload body")
		return
	}
	if len(data) > maxSizeUpload {
		writeLimit(w, "upload body exceeds maxSizeUpload")
		return
	}
	blobID, err := s.store.PutBlob(r.Context(), acct.ID, mediaType, data)
	if err != nil {
		if s.log != nil {
			s.log.Error("httpapi: store upload", "account", acct.ID, "err", err)
		}
		writeProblem(w, http.StatusInternalServerError, "urn:ietf:params:jmap:error:notRequest",
			"could not store the upload")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId": acct.ID,
		"blobId":    blobID,
		"type":      mediaType,
		"size":      len(data),
	}, nil)
}

// handleDownload serves GET /{account}/download/{blobId}/{name}?type=…
// (RFC 8620 §6.2): the bytes, a Content-Type from {type}, and the name
// as the download filename.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	acct := s.authorize(w, r, r.PathValue("account"))
	if acct == nil {
		if s.cfg.Auth.Mode == "none" {
			http.NotFound(w, r)
		}
		return
	}
	blobID := r.PathValue("blobId")
	data, stored, err := s.store.ReadBlob(r.Context(), acct.ID, blobID)
	if errors.Is(err, jmapapi.ErrBlobNotFound) {
		// Unknown and foreign ids answer identically: a probe learns
		// nothing about another account's blobs (FR-M.17, FR-A.11).
		writeProblem(w, http.StatusNotFound, "urn:ietf:params:jmap:error:notFound",
			"no such blob in this account")
		return
	}
	if err != nil {
		if s.log != nil {
			s.log.Error("httpapi: read blob", "account", acct.ID, "err", err)
		}
		writeProblem(w, http.StatusInternalServerError, "urn:ietf:params:jmap:error:notRequest",
			"could not read the blob")
		return
	}
	w.Header().Set("Content-Type", downloadType(r.URL.Query().Get("type"), stored))
	w.Header().Set("Content-Disposition", contentDisposition(r.PathValue("name")))
	// A blob's bytes never change, so the response is safe to cache for
	// as long as the id exists (RFC 8620 §6.2 recommends exactly this).
	w.Header().Set("Cache-Control", "private, immutable, max-age=31536000")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// writeLimit answers a size-cap refusal with the registered limit
// problem type, which must name the limit it applied (RFC 8620 §3.6.1).
func writeLimit(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"type":   "urn:ietf:params:jmap:error:limit",
		"status": http.StatusRequestEntityTooLarge,
		"limit":  "maxSizeUpload",
		"detail": detail,
	}, nil)
}

// writeProblem writes an RFC 7807 problem details body, which RFC 8620
// asks for on every HTTP error response (§6.1, §6.2).
func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	writeJSON(w, status, map[string]any{
		"type":   typ,
		"status": status,
		"detail": detail,
	}, nil)
}

// downloadType picks the Content-Type of a download: what the client's
// {type} asked for (the bytes carry no type of their own, RFC 8620
// §6.2), else the type the upload recorded, else octet-stream. Anything
// that would not survive being a header is skipped rather than sent.
func downloadType(requested, stored string) string {
	for _, candidate := range []string{requested, stored, "application/octet-stream"} {
		if candidate == "" || strings.ContainsAny(candidate, "\r\n") {
			continue
		}
		if mt, _, err := mime.ParseMediaType(candidate); err == nil && mt != "" {
			return candidate
		}
	}
	return "application/octet-stream"
}

// contentDisposition turns the {name} template value into a
// Content-Disposition header: control characters are dropped so a
// filename can never fold a header of its own (NFR-5), and
// mime.FormatMediaType does the quoting and encoding.
func contentDisposition(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "attachment"
	}
	if d := mime.FormatMediaType("attachment", map[string]string{"filename": name}); d != "" {
		return d
	}
	return "attachment"
}
